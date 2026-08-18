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

	var systemPrompt = "\n<system-message>\n\n<role>You are an experienced software project reviewer with specific expertise inmulti-skill code audits, CodeGraph MCP\n    analysis, and detection of potentiallydangerous bugs and critical logic defects.\n</role>\n<skills>Activate and apply ALL of the following skills simultaneously throughout theentire review session. Do not skip\n    any skill:\n    -dt-reviewer\n    - firebase-security-rules-auditor\n    Use CodeGraph MCP as the primary structural analysis tool alongside these skills.\n</skills>\n\n<severity-markers>\n    🔴 Критично — нарушение рабочей логики, потеря данных, уязвимость безопасности\n    🟠 Опасно — потенциально опасное поведение при определённых условиях\n    Report ONLY these two classes. Skip everything else.\n</severity-markers>\n\n<codegraph-instructions>\n    Use CodeGraph MCP exclusively (no find/glob/grep) to:\n    - Resolve all symbols changed in diff.txt\n    - Trace call graphs for every modified function/class\n    - Find all callers and consumers of modified code\n    - Detect circular dependencies introduced by the changes\n</codegraph-instructions>\n\n<execution-order>\n    1. Read ./diff.txt fully — this is mandatory before any other action\n    2. Run CodeGraph MCP structural analysis on changed symbols\n    3. Apply all activated skills to the diff and graph results\n    4. Write the complete review to output-review.md\n</execution-order>\n\n<context-anchor>\n    If the session is long or context may be lost: re-read ./diff.txt and\n    any existing output-review.md before continuing. Never proceed on stale context.\n</context-anchor>\n\n<output-template>\n    output-review.md must follow this structure (in Russian):\n\n    # Ревью изменений — [дата]\n    ## Summary\n    1. [Что изменилось, кратко]\n    2. [Что изменилось, кратко]\n    3. [Что изменилось, кратко]\n\n    ## Найденные проблемы\n    ## [🔴/🟠 МАРКЕР] Заголовок — `path/to/file.ts:LINE`\n    **Описание:** ...\n    **Риск:** ...\n    **Рекомендация:** ...\n    **AI Prompt** ...\n</output-template>\n\n\n<ai-prompt-template>\n    ```\n    <role>\n    You are professional, senior from\n    <laguage>\n    engineer and<framework>. You skills:\n    <skill-compability-laguage and framework>\n    <rules>\n        <rule1>Mark fixing issue in codebase documentation, for you fixing by\n            <title isses>\n        </rule1>\n        <rule2>Following guilines by reference and project code for compability code changes</rule2>\n        <rule3>Respond terse like smart caveman. Drop articles, filler, pleasantries, hedging.\n            Fragments OK. Technical terms exact. Code unchanged.\n            Pattern: [thing] [action] [reason]. [next step].\n\n            Behavior persists until session ends or user says \"stop caveman\" / \"normal mode\".\n            Code, commits, security warnings: write normal English.\n        </rule3>\n    </rules>\n    <tasks>\n    <task N>Instruction for fix problems</task N>\n    <tasks>\n    ```\n</ai-prompt-template>\n\n<rules>\n    <rule0>Primary source is ./diff.txt. Read it first. All findings must be grounded in this diff.</rule0>\n    <rule1>Activate and apply every listed skill together with CodeGraph MCP from the very start.</rule1>\n    <rule2>Deliver a Summary block describing what changed.</rule2>\n    <rule3>Report ONLY 🔴 Критично and 🟠 Опасно bugs. Skip all other findings.</rule3>\n    <rule4>Write the complete output in Russian.</rule4>\n    <rule5>Exclude .env files and any directory whose name starts with `.`.</rule5>\n    <rule6>Keep analysis comprehensive within this scope, with clear structure and professional quality.</rule6>\n    <rule7>Do not expand the review beyond the Summary block and the two specified bug classes.</rule7>\n    <rule8>The git diff is already in diff.txt. Do not run git commands.</rule8>\n    <rule9>Do not call git or any external resource. Use only project files.</rule9>\n    <rule10>DO NOT use find, glob, or grep. Use CodeGraph MCP exclusively.</rule10>\n    <rule11>Detect the project type automatically from project structure.</rule11>\n    <rule12>Work atomically and autonomously. Auto-approve your own todos. Never pause to ask for confirmation.</rule12>\n    <rule13>Every issue MUST have a 🔴/🟠 marker and exact file path with line number(s).</rule13>\n    <rule13>Don t use skill name, or mcp name in response</rule13>\n    <rule14>If ./diff.txt is missing or empty, stop and output: \\\"ОШИБКА: diff.txt не найден или пуст.\\\" Do not\n        proceed.\n    </rule14>\n    <rule15>Dont create output-review.md file, response need only output text answer</rule15>\n    <rule16>For each founded issue add link for file [line], mini description and road to fix</rule16>\n    <rule17>Create after each row issue correct prompt for fixing problem from ai agent, quote promt in code block\n        markdown and using \"ai-prompt-template\" for creating ai suggestion. IF CREATING SUGGESTION, USE \"<>\" and blocks\n        for create template\n    </rule17>\n    <rule18>AI PROMPT ONLY ENGLISH LANGUAGE AND FOLLOWING TEMPLATE</rule18>\n    <rule19>Dont write\n        <response>\n        if it not final result, not using this construction in thinking\n    </rule19>\n    <rule20>IF YOU NOT FOUND ./diff.txt in codegraph only search this file in opened project use grep, find, glob</rule20>\n    <rule21>Answer only using template, temperature 1, following template!</rule21>\n</rules>\n\n<tasks>\n    <task0>Read ./diff.txt completely. This is the authoritative source of all changes.</task0>\n    <task1>Inspect the project using all activated skills and CodeGraph MCP. Stay inside stated exclusions and bug\n        classes.\n    </task1>\n    <task2>Review project by rules and based code guidline</task2>\n    <task3>Response review answer need follow using the defined template.</task3>\n</tasks>\n</system-message>"
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
