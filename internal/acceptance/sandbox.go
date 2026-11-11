package acceptance

import (
	"context"
	"encoding/json"
	"errors"

	assets "github.com/lannokaede/LanAgent/examples/practice"
	"github.com/lannokaede/LanAgent/internal/config"
)

func (v *verifier) sandbox(ctx context.Context, cfg *config.Config) (map[string]any, error) {
	return v.withSession(cfg, "sandbox", "rules", func(sess *session) (map[string]any, error) {
		code, err := assets.Files.ReadFile("context-cancel/v1/correct.go.txt")
		if err != nil {
			return nil, err
		}
		prompt := "请原样使用以下 Go 代码调用 run_go_exercise，禁止修改代码；完成后报告工具返回的 code_hash 和测试状态。\n" + string(code)
		_, err = sess.run(ctx, prompt, "", "-exercise", "context-cancel/v1")
		if err != nil {
			return nil, err
		}
		rows, err := sess.journal()
		if err != nil {
			return nil, err
		}
		m := metrics(rows)
		m["docker_image"] = cfg.Practice.Image
		m["sandbox_calls"] = countTools(rows, "run_go_exercise")
		for _, row := range rows {
			e, _ := row["event"].(map[string]any)
			d, _ := e["Data"].(map[string]any)
			if e["Type"] == "tool_result" {
				result, _ := d["result"].(string)
				var evidence map[string]any
				if json.Unmarshal([]byte(result), &evidence) == nil && evidence["status"] == "passed" && evidence["code_hash"] != "" {
					m["code_hash"] = evidence["code_hash"]
					return m, nil
				}
			}
		}
		return m, errors.New("no hash-bound passing Docker result observed")
	})
}
