package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/google/uuid"
)

type RepoAI struct {
	URL                string `json:"url"`
	GitToken           string `json:"gitToken"`
	GitBranch          string `json:"gitBranch"`
	OpencodeUrl        string `json:"opencodeUrl"`
	OpencodeModel      string `json:"opencodeModel"`
	OpencodeSecret     string `json:"opencodeSecret"`
	Prompt             string `json:"prompt"`
	CommentGitToken    string `json:"commentGitToken"`
	CleanAfterReview   bool   `json:"cleanAfterReview"`
	SendResponseForGit bool   `json:"sendResponseForGit"`
}

// getBaseRepoURL возвращает базовый URL для клонирования (без частей MR/PR)
func getBaseRepoURL(rawURL string) string {
	if idx := strings.Index(rawURL, "/pull/"); idx != -1 {
		return rawURL[:idx] + ".git"
	}
	if idx := strings.Index(rawURL, "/-/merge_requests/"); idx != -1 {
		return rawURL[:idx] + ".git"
	}
	// Если это обычный URL репозитория, возвращаем как есть (добавляем .git если нужно)
	if !strings.HasSuffix(rawURL, ".git") {
		return rawURL + ".git"
	}
	return rawURL
}

func applyTokenToURL(rawURL, token string) string {
	if token == "" {
		return rawURL
	}

	if strings.HasPrefix(rawURL, "https://") {
		return strings.Replace(rawURL, "https://", "https://git:"+token+"@", 1)
	}

	if strings.HasPrefix(rawURL, "http://") {
		return strings.Replace(rawURL, "http://", "http://git:"+token+"@", 1)
	}

	return rawURL
}

// parseMergeRequestURL извлекает хост, полный путь проекта и номер MR из URL
func parseMergeRequestURL(rawURL string) (host, projectPath, mrID string, err error) {
	// Ищем разделители в порядке приоритета
	separators := []string{"/-/merge_requests/", "/merge_requests/", "/pull/"}
	var base, id string
	for _, sep := range separators {
		if idx := strings.Index(rawURL, sep); idx != -1 {
			base = rawURL[:idx]
			id = rawURL[idx+len(sep):]
			break
		}
	}
	if base == "" || id == "" {
		return "", "", "", fmt.Errorf("не удалось разобрать URL MR: %s", rawURL)
	}

	// Парсим базовую часть для получения хоста и пути
	u, err := url.Parse(base)
	if err != nil {
		return "", "", "", err
	}
	host = u.Host
	projectPath = strings.TrimPrefix(u.Path, "/")
	if projectPath == "" {
		return "", "", "", fmt.Errorf("пустой путь проекта в URL: %s", rawURL)
	}
	return host, projectPath, id, nil
}

// FetchMergeRequestDiff получает diff через API для любого Git-хоста
// Поддерживает GitHub, GitLab (включая self-hosted)
func FetchMergeRequestDiff(rawURL, token string) (string, error) {
	host, projectPath, mrID, err := parseMergeRequestURL(rawURL)
	if err != nil {
		return "", err
	}

	// Определяем тип хоста и строим API-запрос
	var apiURL string
	var headers = map[string]string{"Accept": "application/json"}

	// Проверяем, является ли хост GitHub (или GitHub Enterprise)
	if strings.Contains(host, "github.com") || strings.Contains(host, "github") {
		// GitHub Enterprise может иметь API на /api/v3, но для простоты используем api.github.com для облачного
		if host == "github.com" {
			apiURL = fmt.Sprintf("https://api.github.com/repos/%s/pulls/%s", projectPath, mrID)
		} else {
			// GitHub Enterprise: API обычно на /api/v3
			apiURL = fmt.Sprintf("https://%s/api/v3/repos/%s/pulls/%s", host, projectPath, mrID)
		}
		if token != "" {
			headers["Authorization"] = "Bearer " + token
		}
	} else {
		// Все остальное считаем GitLab (включая self-hosted и gitlab.com)
		// Кодируем путь проекта для URL (заменяем / на %2F)
		encodedPath := strings.ReplaceAll(projectPath, "/", "%2F")
		apiURL = fmt.Sprintf("https://%s/api/v4/projects/%s/merge_requests/%s/diffs", host, encodedPath, mrID)
		if token != "" {
			headers["PRIVATE-TOKEN"] = token
		}
	}

	// Выполняем HTTP-запрос
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := newAPIClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(body))
	}

	// Парсим ответ в зависимости от хоста
	var diffContent string
	if strings.Contains(host, "github.com") || strings.Contains(host, "github") {
		// GitHub возвращает массив файлов с полем "patch"
		var files []struct {
			Patch string `json:"patch"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
			return "", err
		}
		for _, f := range files {
			diffContent += f.Patch + "\n"
		}
	} else {
		// GitLab возвращает массив объектов с полем "diff"
		var diffs []struct {
			Diff string `json:"diff"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&diffs); err != nil {
			return "", err
		}
		for _, d := range diffs {
			diffContent += d.Diff + "\n"
		}
	}

	if diffContent == "" {
		return "", fmt.Errorf("дифф пуст (возможно, MR не содержит изменений)")
	}
	return diffContent, nil
}

func newAPIClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// postReviewComment публикует ревью как комментарий к MR (GitLab) или PR (GitHub)
func postReviewComment(rawURL, token, review string) error {
	if token == "" || review == "" {
		return nil
	}

	host, projectPath, mrID, err := parseMergeRequestURL(rawURL)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(map[string]string{"body": review})
	if err != nil {
		return err
	}

	var apiURL string
	headers := map[string]string{"Accept": "application/json", "Content-Type": "application/json"}

	if strings.Contains(host, "github") {
		// GitHub: комментарий к PR — это комментарий к issue
		if host != "github.com" {
			apiURL = fmt.Sprintf("https://%s/api/v3/repos/%s/issues/%s/comments", host, projectPath, mrID)
		} else {
			apiURL = fmt.Sprintf("https://api.github.com/repos/%s/issues/%s/comments", projectPath, mrID)
		}
		headers["Authorization"] = "Bearer " + token
	} else {
		// GitLab (включая self-hosted): заметка к MR
		encodedPath := strings.ReplaceAll(projectPath, "/", "%2F")
		apiURL = fmt.Sprintf("https://%s/api/v4/projects/%s/merge_requests/%s/notes", host, encodedPath, mrID)
		headers["PRIVATE-TOKEN"] = token
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := newAPIClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API комментариев вернул статус %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// FetchMergeRequestSourceBranch получает source-ветку MR/PR через API
func FetchMergeRequestSourceBranch(rawURL, token string) (string, error) {
	host, projectPath, mrID, err := parseMergeRequestURL(rawURL)
	if err != nil {
		return "", err
	}

	var apiURL string
	headers := map[string]string{"Accept": "application/json"}

	if strings.Contains(host, "github") {
		if host == "github.com" {
			apiURL = fmt.Sprintf("https://api.github.com/repos/%s/pulls/%s", projectPath, mrID)
		} else {
			apiURL = fmt.Sprintf("https://%s/api/v3/repos/%s/pulls/%s", host, projectPath, mrID)
		}
		if token != "" {
			headers["Authorization"] = "Bearer " + token
		}
	} else {
		encodedPath := strings.ReplaceAll(projectPath, "/", "%2F")
		apiURL = fmt.Sprintf("https://%s/api/v4/projects/%s/merge_requests/%s", host, encodedPath, mrID)
		if token != "" {
			headers["PRIVATE-TOKEN"] = token
		}
	}

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := newAPIClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(body))
	}

	if strings.Contains(host, "github") {
		var pr struct {
			Head struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
			return "", err
		}
		if pr.Head.Ref == "" {
			return "", fmt.Errorf("PR не содержит source-ветку (head.ref)")
		}
		return pr.Head.Ref, nil
	}

	var mr struct {
		SourceBranch string `json:"source_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return "", err
	}
	if mr.SourceBranch == "" {
		return "", fmt.Errorf("MR не содержит source-ветку")
	}
	return mr.SourceBranch, nil
}

func getCloneTokenizedGitUrl(data RepoAI) string {
	return applyTokenToURL(getBaseRepoURL(data.URL), data.GitToken)
}

func writeOpenCodeConfig(repoPath string, data RepoAI) error {
	config := make(map[string]interface{})
	config["$schema"] = "https://opencode.ai/config.json"

	// Определяем, является ли opencodeUrl кастомным URL-адресом
	isCustomURL := strings.HasPrefix(data.OpencodeUrl, "http://") ||
		strings.HasPrefix(data.OpencodeUrl, "https://")

	if isCustomURL {
		// ---- Кастомный OpenAI-совместимый прокси ----
		providerName := "custom"
		provider := map[string]interface{}{
			"npm":  "@ai-sdk/openai-compatible",
			"name": "Custom Proxy",
			"options": map[string]interface{}{
				"baseURL": data.OpencodeUrl,
				"apiKey":  data.OpencodeSecret,
			},
			"models": map[string]interface{}{
				data.OpencodeModel: map[string]string{"name": data.OpencodeModel},
			},
			"skills": strings.ReplaceAll("[\"firebase-security-rules-auditor\", \"dt-reviewer\"]", "\\", ""),
		}
		config["provider"] = map[string]interface{}{
			providerName: provider,
		}
		config["model"] = providerName + "/" + data.OpencodeModel
	} else {
		// ---- Стандартный провайдер (Anthropic, OpenAI и т.д.) ----
		if data.OpencodeUrl != "" {
			config["provider"] = data.OpencodeUrl
		}
		if data.OpencodeModel != "" {
			if data.OpencodeUrl != "" {
				config["model"] = data.OpencodeUrl + "/" + data.OpencodeModel
			} else {
				config["model"] = data.OpencodeModel
			}
		}
		// Если передан секрет, добавляем его на верхний уровень (если поддерживается)
		if data.OpencodeSecret != "" {
			config["apiKey"] = data.OpencodeSecret
		}
	}

	// Записываем JSON в файл
	filePath := filepath.Join(repoPath, "opencode.json")
	dataBytes, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, dataBytes, 0644)
}

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

func prompt(data RepoAI, dir string, w http.ResponseWriter) []byte {
	args := []string{"run"}

	err := writeOpenCodeConfig(dir, data)
	if err != nil {
		fmt.Print("Ошибка создания конфига")
		return nil
	}
	args = append(args, "--auto")
	//args = append(args, "--session", uuid.Must(uuid.NewV7()).String())

	var systemPrompt = "<system-message>\n\n    <!-- ═══════════════════════════════════════════\n         IDENTITY LAYER\n    ═══════════════════════════════════════════ -->\n    <role>\n        <title>Senior Software Project Reviewer / Code Audit Orchestrator</title>\n        <persona>Опытный, строгий, лаконичный ревьюер. Не тратит слов на воду, фокусируется только на критичных находках. Профессиональный тон, без лести и хеджирования.</persona>\n        <expertise>\n            Multi-skill code audit, CodeGraph MCP structural analysis, dt-reviewer skill,\n            firebase-security-rules-auditor skill, detection of dangerous bugs and critical logic defects.\n        </expertise>\n    </role>\n\n    <!-- ═══════════════════════════════════════════\n         CONTEXT LAYER\n    ═══════════════════════════════════════════ -->\n    <context>\n        <domain>Code review / security audit по git-диффу проекта</domain>\n        <background>\n            Основной источник правды — ./diff.txt (уже сформированный git diff). Тип проекта определяется\n            автоматически по структуре репозитория.\n        </background>\n        <constraints>\n            <time>Работа атомарна и автономна, без пауз на подтверждение</time>\n            <stack>Активировать одновременно: dt-reviewer, firebase-security-rules-auditor, CodeGraph MCP</stack>\n            <budget>Только 2 класса находок: 🔴 Критично и 🟠 Опасно, всё остальное пропускается</budget>\n            <forbidden>\n                - Не использовать find/glob/grep (кроме fallback по rule20)\n                - Не вызывать git или внешние ресурсы\n                - Не создавать файл output-review.md, только текстовый ответ\n                - Не упоминать названия skills/mcp в ответе\n                - Не выходить за рамки Summary + два класса багов\n            </forbidden>\n        </constraints>\n        <existing_artifacts>./diff.txt — обязательный для чтения перед любым действием</existing_artifacts>\n    </context>\n\n    <!-- ═══════════════════════════════════════════\n         RULES LAYER\n    ═══════════════════════════════════════════ -->\n    <rules>\n        <global>\n            <rule>Первичный источник — ./diff.txt. Все находки должны быть обоснованы диффом</rule>\n            <rule>Активировать все указанные skills и CodeGraph MCP с самого начала одновременно</rule>\n            <rule>Отчитываться ТОЛЬКО о 🔴 Критично и 🟠 Опасно находках</rule>\n            <rule>Полный ответ пишется на русском языке</rule>\n            <rule>Исключить .env файлы и любые директории, начинающиеся с точки</rule>\n            <rule>Каждая находка обязана иметь маркер 🔴/🟠, точный путь к файлу и номер строки</rule>\n            <rule>Не использовать find, glob, grep — только CodeGraph MCP (кроме fallback, если diff.txt не найден в CodeGraph)</rule>\n            <rule>Тип проекта определяется автоматически по структуре</rule>\n            <rule>Работать атомарно и автономно, авто-подтверждать свои todo, не спрашивать разрешения</rule>\n            <rule>Не использовать названия skill/mcp в ответе</rule>\n            <rule>Если ./diff.txt отсутствует или пуст — остановиться и вывести: \"ОШИБКА: diff.txt не найден или пуст.\" Не продолжать</rule>\n            <rule>Не создавать файл output-review.md — только текстовый ответ в чат</rule>\n            <rule>Для каждой найденной проблемы: ссылка на файл[строка], краткое описание, путь исправления</rule>\n            <rule>Не писать тег ответа, если это не финальный результат</rule>\n            <rule>Отвечать только по шаблону, temperature 1, строго следуя структуре</rule>\n        </global>\n        <formatting>\n            Response by template (output_format below), строго на русском, без лишнего текста\n        </formatting>\n        <behavior>\n            Профессиональный и конкретный ответ, никакого лишнего текста, никаких пауз на подтверждение\n        </behavior>\n    </rules>\n\n    <!-- ═══════════════════════════════════════════\n         MEMORY LAYER\n    ═══════════════════════════════════════════ -->\n    <memory>\n        <main priority=\"high\">\n            <title>Контекст-якорь для длинных сессий</title>\n            <steps>\n                <step id=\"1\">Если сессия длинная или контекст может быть потерян — перечитать ./diff.txt</step>\n                <step id=\"2\">Перечитать существующий текстовый результат ревью перед продолжением</step>\n                <step id=\"3\">Никогда не продолжать на устаревшем контексте</step>\n            </steps>\n        </main>\n        <shared_state>\n            <var name=\"project_name\"></var>\n            <var name=\"current_step\">read_diff</var>\n            <var name=\"status\">idle</var>\n        </shared_state>\n        <artifacts>\n            <artifact>Содержимое diff.txt</artifact>\n            <artifact>Результаты CodeGraph MCP анализа (call graphs, callers, circular deps)</artifact>\n            <artifact>Список найденных 🔴/🟠 проблем с путями и строками</artifact>\n        </artifacts>\n        <history>\n            После завершения ревью — фиксировать краткую сводку найденных проблем для последующих сессий\n        </history>\n    </memory>\n\n    <!-- ═══════════════════════════════════════════\n         TASKS LAYER\n    ═══════════════════════════════════════════ -->\n    <tasks>\n\n        <task id=\"1\" priority=\"high\" depends_on=\"\">\n            <description>Прочитать ./diff.txt полностью — обязательно перед любым другим действием</description>\n            <goal>Получить авторитетный источник всех изменений</goal>\n            <input>./diff.txt</input>\n            <expected_output>Полное содержимое диффа в контексте; если файл пуст/не найден — ошибка и стоп</expected_output>\n        </task>\n\n        <task id=\"2\" priority=\"high\" depends_on=\"1\">\n            <description>\n                Запустить структурный анализ CodeGraph MCP по изменённым символам: резолв символов,\n                трассировка call graph для каждой изменённой функции/класса, поиск всех вызывающих/потребителей,\n                детекция циклических зависимостей\n            </description>\n            <goal>Построить граф влияния изменений</goal>\n            <input>Изменённые символы из diff.txt</input>\n            <expected_output>Граф вызовов, список потребителей, найденные циклические зависимости</expected_output>\n        </task>\n\n        <task id=\"3\" priority=\"high\" depends_on=\"2\">\n            <description>\n                Применить все активированные skills (dt-reviewer, firebase-security-rules-auditor) к диффу и\n                результатам графа, искать только 🔴 Критично и 🟠 Опасно проблемы\n            </description>\n            <goal>Список находок с маркерами, путями, строками, описанием, риском и рекомендацией</goal>\n            <input>Результаты task 1 и task 2</input>\n            <expected_output>Структурированный список найденных проблем</expected_output>\n        </task>\n\n        <task id=\"4\" priority=\"medium\" depends_on=\"3\">\n            <description>Сформировать финальный ответ строго по output_format, без создания файлов</description>\n            <goal>Готовый текстовый отчёт ревью на русском языке</goal>\n            <input>Артефакт из task 3</input>\n            <expected_output>Финальный текст ответа пользователю</expected_output>\n        </task>\n\n    </tasks>\n\n    <!-- ═══════════════════════════════════════════\n         OUTPUT LAYER\n    ═══════════════════════════════════════════ -->\n    <output_format>\n        <type>report</type>\n        <language>ru</language>\n        <structure>\n            # Ревью изменений — [дата]\n\n            ## Summary\n            1. [Что изменилось, кратко]\n            2. [Что изменилось, кратко]\n            3. [Что изменилось, кратко]\n\n            ## Найденные проблемы\n            ## [🔴/🟠 МАРКЕР] Заголовок — `path/to/file.ts:LINE`\n            **Описание:** ...\n            **Риск:** ...\n            **Рекомендация:** ...\n        </structure>\n        <length>compact, только Summary и найденные 🔴/🟠 проблемы, без лишнего текста</length>\n    </output_format>\n\n    <!-- ═══════════════════════════════════════════\n         EXECUTION LAYER\n    ═══════════════════════════════════════════ -->\n    <steps>\n\n        <step id=\"1\" name=\"PLAN\">\n            <action>\n                1. Прочитать ./diff.txt полностью (rule0, rule14)\n                2. Если файл отсутствует/пуст — вывести \"ОШИБКА: diff.txt не найден или пуст.\" и остановиться\n                3. Если diff.txt не найден через CodeGraph — искать его в открытом проекте через grep/find/glob (rule20)\n                4. Обновить shared_state.current_step\n            </action>\n            <output>Содержимое diff.txt в контексте либо стоп-ошибка</output>\n        </step>\n\n        <step id=\"2\" name=\"BUILD\">\n            <action>\n                Запустить CodeGraph MCP структурный анализ и применить активированные skills:\n            </action>\n            <subagent>\n                <role>\n                    inherits: reviewer role + expertise\n                    specific: структурный аудит через CodeGraph MCP + security/logic аудит через dt-reviewer и\n                    firebase-security-rules-auditor\n                </role>\n                <input>diff.txt, изменённые символы, граф вызовов</input>\n                <instructions>\n                    Резолвить все изменённые символы, трассировать call graph, найти вызывающих/потребителей,\n                    выявить циклические зависимости, найти только 🔴 Критично и 🟠 Опасно проблемы, исключая\n                    .env и dot-директории\n                </instructions>\n                <output>\n                    <format>Список найденных проблем с маркером, путём, строкой, описанием, риском и рекомендацией</format>\n                    <destination>memory.artifacts</destination>\n                </output>\n                <constraints>\n                    <max_iterations>3</max_iterations>\n                    <scope>Только diff.txt и связанный граф изменений, не выходить за рамки двух классов багов</scope>\n                </constraints>\n            </subagent>\n        </step>\n\n        <step id=\"3\" name=\"VERIFICATION\">\n            <action>\n                1. Сверить найденные проблемы с изменениями в diff.txt (каждая находка обоснована диффом)\n                2. Проверить, что каждая проблема имеет маркер 🔴/🟠, путь и строку\n                3. Сохранить результат в memory.history\n            </action>\n            <criteria>\n                <criterion>Все находки относятся только к 🔴 Критично / 🟠 Опасно</criterion>\n                <criterion>Формат ответа соответствует output_format</criterion>\n                <criterion>Нет нарушений правил (rules.global)</criterion>\n                <criterion>Ответ полностью на русском</criterion>\n            </criteria>\n            <approver>user</approver>\n            <on_approve>proceed to step 4</on_approve>\n            <on_reject>return to step 2 with rejection comment</on_reject>\n        </step>\n\n        <step id=\"4\" name=\"NEXT_TASK\">\n            <action>\n                1. Обновить memory.artifacts итоговым списком проблем\n                2. Обновить memory.history краткой сводкой\n                3. Если задачи выполнены — перейти к DONE\n            </action>\n        </step>\n\n        <step id=\"error\" name=\"FALLBACK\">\n            <trigger>\n                diff.txt не найден/пуст, CodeGraph MCP недоступен, или иная ошибка анализа\n            </trigger>\n            <action>\n                1. Log причину сбоя в memory.history\n                2. Если diff.txt не найден в CodeGraph — попытаться найти файл в проекте (rule20)\n                3. Если после retry ошибка сохраняется — вывести \"ОШИБКА: diff.txt не найден или пуст.\" и стоп\n            </action>\n            <output>\n                Отчёт об ошибке: что не сработало, где, почему, варианты решения\n            </output>\n        </step>\n\n        <step id=\"5\" name=\"DONE\">\n            <action>\n                1. Собрать финальный результат из memory.artifacts\n                2. Оформить строго по output_format (Summary + Найденные проблемы)\n                3. Вывести финальный отчёт пользователю как текст (без создания файлов)\n            </action>\n            <report>\n                <completed_tasks/>\n                <failed_tasks/>\n                <artifacts/>\n                <next_steps>Рекомендации по дальнейшим действиям</next_steps>\n            </report>\n        </step>\n\n    </steps>\n\n</system-message>"
	if data.Prompt != "" {
		args = append(args, systemPrompt+"<user-message>"+data.Prompt+"</user-message>")
	} else {
		args = append(args, systemPrompt)
	}

	cmd := exec.Command("opencode", args...)

	fmt.Println(cmd.String())

	cmd.Dir = "/" + dir
	if _, err := exec.LookPath("opencode"); err != nil {
		home, _ := os.UserHomeDir()
		fallback := filepath.Join(home, ".opencode", "bin", "opencode")
		cmd = exec.Command(fallback, args...)
		cmd.Dir = "/" + dir
	}

	fmt.Print(cmd.String())

	flusher, _ := w.(http.Flusher)
	fw := flushWriter{w: w, f: flusher}
	var buf bytes.Buffer
	var wg sync.WaitGroup

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(&buf, &ansiStripper{r: stdout}) }()
	go func() { defer wg.Done(); io.Copy(io.Discard, stderr) }()

	err = cmd.Start()
	if err != nil {
		http.Error(w, "Ошибка запуска opencode: "+err.Error(), http.StatusInternalServerError)
		return nil
	}
	wg.Wait()
	cmd.Wait()

	text := extractResponse(buf.String())
	if text == "" {
		text = buf.String()
	}
	fw.Write([]byte(text))

	return []byte(text)
}

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b[=3-9h-l]|\x1b\([B0]`)

type ansiStripper struct{ r io.Reader }

func (a *ansiStripper) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		p2 := ansiRegexp.ReplaceAll(p[:n], nil)
		copy(p, p2)
		return len(p2), err
	}
	return n, err
}

func extractResponse(s string) string {
	start := strings.Index(s, "<response>")
	if start == -1 {
		return ""
	}
	start += len("<response>")
	end := strings.Index(s[start:], "</response>")
	if end == -1 {
		return s[start:]
	}
	return strings.TrimSpace(s[start : start+end])
}

func isMergeRequestURL(url string) bool {
	return strings.Contains(url, "/pull/") || strings.Contains(url, "/merge_requests/")
}

func execHandler(w http.ResponseWriter, r *http.Request) {

	if r.Method != http.MethodPost {
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	var data RepoAI
	if errData := json.NewDecoder(r.Body).Decode(&data); errData != nil {
		http.Error(w, "Invalid JSON: "+errData.Error(), http.StatusBadRequest)
		return
	} else {
		fmt.Println("URL:" + data.URL)
	}
	fmt.Println(exec.Command("git", "clone", getCloneTokenizedGitUrl(data)))

	gitClone := exec.Command("git", "clone", getCloneTokenizedGitUrl(data))
	outGitClone, errGitClone := gitClone.Output()

	fmt.Printf("Output:\n%s", outGitClone)
	fmt.Printf("Err:\n%s", errGitClone)

	var dirProjectOld = strings.TrimSuffix(filepath.Base(getBaseRepoURL(data.URL)), ".git")

	if data.GitBranch == "" && isMergeRequestURL(data.URL) {
		sourceBranch, err := FetchMergeRequestSourceBranch(data.URL, data.GitToken)
		if err != nil {
			http.Error(w, "Failed to fetch source branch: "+err.Error(), http.StatusInternalServerError)
			return
		}
		data.GitBranch = sourceBranch
		fmt.Println("Source branch:", data.GitBranch)
	}

	if data.GitBranch != "" {
		fmt.Printf("Branch: %s\n", data.GitBranch)

		errCheckout := exec.Command("git", "-C", dirProjectOld, "checkout", data.GitBranch).Run()
		if errCheckout != nil {
			http.Error(w, fmt.Sprintf("Error checkout branch: %v", errCheckout), http.StatusInternalServerError)
			return
		}

		errPull := exec.Command("git", "-C", dirProjectOld, "pull", "origin", data.GitBranch).Run()
		if errPull != nil {
			http.Error(w, fmt.Sprintf("Error pull branch: %v", errPull), http.StatusInternalServerError)
			return
		}
		fmt.Println("Branch checked out and pulled:", data.GitBranch)
	}

	id7 := uuid.Must(uuid.NewV7())

	var dirProject = strings.TrimSuffix(filepath.Base(getBaseRepoURL(data.URL)), ".git") + "_" + id7.String()

	fmt.Printf(exec.Command("mv", dirProjectOld, dirProject).String())

	errMv := exec.Command("mv", dirProjectOld, dirProject).Run()

	if errMv != nil {
		http.Error(w, fmt.Sprintf("Error mv: %v", errMv), http.StatusInternalServerError)
		return
	}

	var diffFilePath string

	if isMergeRequestURL(data.URL) {
		diffContent, err := FetchMergeRequestDiff(data.URL, data.GitToken)
		if err != nil {
			http.Error(w, "Failed to fetch MR diff: "+err.Error(), http.StatusInternalServerError)
			return
		}
		diffFilePath = filepath.Join(dirProject, "diff.txt")
		if err := os.WriteFile(diffFilePath, []byte(diffContent), 0644); err != nil {
			http.Error(w, "Failed to write diff file: "+err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Println("Diff saved to", diffFilePath)
	}

	if errGitClone != nil {
		http.Error(w, fmt.Sprintf("Error cloning repository: %v", errGitClone), http.StatusInternalServerError)
		return
	}

	errCp := exec.Command("cp", "-r", ".opencode", dirProject).Run()

	if errCp != nil {
		http.Error(w, fmt.Sprintf("Error copy skills: %v", errCp), http.StatusInternalServerError)
		return
	}

	errCd := exec.Command("codegraph", "init", dirProject).Run()

	if errCd != nil {
		http.Error(w, fmt.Sprintf("Error create codegraph: %v", errCd), http.StatusInternalServerError)
		return
	}

	review := prompt(data, dirProject, w)

	if data.SendResponseForGit == true {
		if data.CommentGitToken != "" {
			text := string(review)
			if text == "" {
				fmt.Printf("\n\n[WARN] Empty review, comment not posted.\n")
			} else if err := postReviewComment(data.URL, data.CommentGitToken, text); err != nil {
				fmt.Printf("\n\n[ERROR] Comment posting failed: %v\n", err)
			} else {
				fmt.Printf("\n\n[OK] Review comment posted.\n")
			}
		}
	}

	if data.CleanAfterReview == true {
		errRm := exec.Command("rm", "-rf", dirProject).Run()

		if errRm != nil {
			http.Error(w, fmt.Sprintf("Error clear dir: %v", errRm), http.StatusInternalServerError)
			return
		}
	}

	fmt.Fprint(w, review)
}

func main() {
	http.HandleFunc("/exec", execHandler)
	fmt.Println("Server on :8080")
	http.ListenAndServe(":8082", nil)
}
