FROM golang:1.26.9-bookworm
ENV GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOCACHE=/opt/go-cache
RUN mkdir -p /opt/warm && printf 'module warm\n\ngo 1.26.0\n' > /opt/warm/go.mod \
 && printf 'package warm\nimport("context";"sync";"time";"fmt";"errors";"sort";"math";"testing")\nfunc TestWarm(t *testing.T){_ = context.Background();_ = sync.Mutex{};_ = time.Second;_ = fmt.Sprint(1);_ = errors.New("warm");sort.Ints([]int{});_ = math.Pi}\n' > /opt/warm/warm_test.go \
 && cd /opt/warm && go test -race ./... && rm -rf /opt/warm \
 && chmod -R a+rX /opt/go-cache
COPY entrypoint.sh /usr/local/bin/practice-entrypoint
RUN chmod 0555 /usr/local/bin/practice-entrypoint
USER 10001:10001
WORKDIR /workspace
ENV GOCACHE=/tmp/go-cache GOTMPDIR=/tmp GOENV=off GOPATH=/tmp/go
ENTRYPOINT ["/usr/local/bin/practice-entrypoint"]
