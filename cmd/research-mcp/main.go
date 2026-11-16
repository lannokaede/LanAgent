// Command research-mcp serves independently deployable research tools. It does
// not run a model loop or require chat credentials.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/mcphttp"
	"github.com/lannokaede/LanAgent/internal/rag"
	"github.com/lannokaede/LanAgent/internal/research"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e := run(ctx, os.Args[1:], os.Stderr); e != nil && !errors.Is(e, context.Canceled) {
		fmt.Fprintln(os.Stderr, "Error:", e)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, status io.Writer) error {
	if len(args) > 0 && args[0] == "index" {
		return runIndex(ctx, args[1:], status)
	}
	flags := flag.NewFlagSet("research-mcp", flag.ContinueOnError)
	flags.SetOutput(status)
	maxSearch := flags.Int("max-search-calls", 20, "persistent provider search request limit for this data directory")
	maxFetch := flags.Int("max-fetch-calls", 100, "persistent fetch request limit for this data directory")
	dir := flags.String("data-dir", ".runtime/research", "private persistent data directory")
	ns := flags.String("namespace", "research-go", "namespace prefix, isolated from the original knowledge base")
	profile := flags.String("config", "configs/interview", "embedding and Milvus configuration directory")
	knowledge := flags.Bool("knowledge", false, "connect embedding and Milvus")
	write := flags.Bool("allow-knowledge-write", false, "register commit and cancel tools, run durable jobs")
	initStore := flags.Bool("init-store", false, "explicitly initialize the configured vector collection")
	web := flags.String("web", "official", "official (no external key) or firecrawl")
	hosts := flags.String("allowed-hosts", "go.dev,redis.io,www.postgresql.org,nowcoder.com,juejin.cn", "public source host allowlist; subdomains included")
	listen := flags.String("listen", "", "Streamable HTTP address; empty uses stdio")
	tokenEnv := flags.String("token-env", "LANAGENT_RESEARCH_TOKEN", "HTTP authentication token environment variable")
	remote := flags.Bool("allow-remote", false, "allow binding outside loopback; deploy behind TLS reverse proxy")
	origins := flags.String("allowed-origins", "", "exact comma-separated HTTP Origin allowlist")
	maxConcurrent := flags.Int("http-concurrency", 8, "maximum active POST requests and actual tool callbacks; GET SSE has a separate equal limit")
	requestTimeout := flags.Duration("http-request-timeout", time.Minute, "HTTP request and tool execution timeout; excludes GET SSE lifetime")
	sessionTimeout := flags.Duration("http-session-timeout", 5*time.Minute, "idle MCP session timeout")
	shutdownTimeout := flags.Duration("http-shutdown-timeout", 5*time.Second, "HTTP drain budget before cancellation and forced connection close")
	if e := flags.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if *maxSearch < 1 || *maxFetch < 1 || flags.NArg() != 0 || *write && !*knowledge || *initStore && !*knowledge {
		return errors.New("use named flags; write/init-store require -knowledge")
	}
	if *maxConcurrent < 1 || *maxConcurrent > 1024 || *requestTimeout <= 0 || *sessionTimeout <= 0 || *shutdownTimeout <= 0 {
		return errors.New("http-concurrency must be 1..1024 and HTTP timeouts must be positive")
	}
	token := ""
	if *listen != "" {
		host, _, e := net.SplitHostPort(*listen)
		if e != nil {
			return errors.New("listen requires host:port")
		}
		ip := net.ParseIP(host)
		if !*remote && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("bind loopback or explicitly use -allow-remote behind TLS")
		}
		token = os.Getenv(*tokenEnv)
		if len(token) < 24 {
			return errors.New("HTTP token environment variable must contain at least 24 characters")
		}
	}
	opts := research.Options{MaxSearchCalls: *maxSearch, MaxFetchCalls: *maxFetch, Directory: *dir, Namespace: *ns, AllowedHosts: strings.Split(*hosts, ","), AllowWrite: *write}
	switch *web {
	case "official":
	case "firecrawl":
		opts.Web = research.NewFirecrawl(os.Getenv("FIRECRAWL_API_KEY"))
	default:
		return errors.New("web must be official or firecrawl")
	}
	if *knowledge {
		cfg, e := config.Load(*profile)
		if e != nil {
			return e
		}
		engine, store, e := rag.FromConfig(cfg)
		if e != nil {
			return e
		}
		defer engine.Close()
		check, cancel := context.WithTimeout(ctx, 30*time.Second)
		if *initStore {
			e = store.Init(check)
		} else {
			e = store.Check(check)
		}
		cancel()
		if e != nil {
			return e
		}
		opts.Indexer = engine
		opts.Binding = cfg.RAG.MilvusURL + "|" + cfg.RAG.Collection + "|" + cfg.Embedding.BaseURL + "|" + cfg.Embedding.Model + fmt.Sprint(cfg.Embedding.Dimensions)
	}
	service, e := research.Open(ctx, opts)
	if e != nil {
		return e
	}
	defer service.Close()
	server := service.Server()
	if *listen == "" {
		fmt.Fprintln(status, "LanAgent research MCP: stdio; data is private and persistent")
		return server.Run(ctx, &mcp.StdioTransport{})
	}
	transport, e := mcphttp.New(server, mcphttp.Options{
		Token: token, AllowedOrigins: strings.Split(*origins, ","), MaxConcurrent: *maxConcurrent,
		RequestTimeout: *requestTimeout, SessionTimeout: *sessionTimeout, ShutdownTimeout: *shutdownTimeout,
	})
	if e != nil {
		return e
	}
	defer transport.Close()
	listener, e := net.Listen("tcp", *listen)
	if e != nil {
		return e
	}
	fmt.Fprintln(status, "LanAgent research MCP:", listener.Addr().String(), "/mcp")
	return transport.Serve(ctx, listener)
}

// Index maintenance runs without a chat model, embedding service or vector DB.
func runIndex(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "refresh" {
		return errors.New("use research-mcp index refresh")
	}
	f := flag.NewFlagSet("index refresh", flag.ContinueOnError)
	f.SetOutput(out)
	dir := f.String("data-dir", ".runtime/research", "research directory containing web-index.db")
	pages := f.Int("max-pages", 100, "maximum attempted pages (1..1000)")
	depth := f.Int("max-depth", 2, "maximum link discovery depth (0..5)")
	if e := f.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if f.NArg() != 0 || *pages < 1 || *pages > 1000 || *depth < 0 || *depth > 5 {
		return errors.New("use named flags; max-pages 1..1000, max-depth 0..5")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	index, e := research.OpenSiteIndex(*dir)
	if e != nil {
		return e
	}
	defer index.Close()
	report, e := index.RefreshOfficial(ctx, *pages, *depth)
	if encodeErr := json.NewEncoder(out).Encode(report); encodeErr != nil {
		return encodeErr
	}
	return e
}
