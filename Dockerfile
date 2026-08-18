FROM golang:1.22 AS builder

WORKDIR /src

COPY src/* ./
COPY .opencode /.opencode
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /app .

FROM node:21 AS main

COPY --from=builder /app /app
COPY --from=builder /.opencode /.opencode

RUN apt update
RUN apt install git -y && apt install curl -y
RUN git config --global http.sslVerify false
RUN curl -fsSL https://opencode.ai/install | bash
ENV PATH="/root/.opencode/bin:${PATH}"
RUN npm i -g @colbymchenry/codegraph -y
RUN codegraph install -t opencode -l global -y

ENTRYPOINT ["/app"]

