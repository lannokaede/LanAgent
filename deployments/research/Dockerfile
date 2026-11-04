FROM golang:1.26.9-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/lanagent-research-mcp ./cmd/research-mcp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/lanagent-research-mcp /usr/local/bin/lanagent-research-mcp
COPY --from=build /src/configs/interview /app/configs/interview
WORKDIR /app
EXPOSE 18090
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/lanagent-research-mcp"]
CMD ["-data-dir", "/data", "-listen", "0.0.0.0:18090", "-allow-remote"]
