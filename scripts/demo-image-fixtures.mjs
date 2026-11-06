// Offline service responses only. File access, SQLite, checkpoints and interview
// validation remain the real Go implementation. Never read personal config here.
import assert from "node:assert/strict";
import { createServer } from "node:http";

export const fixture = {
  file: "note.md",
  fileContent:
    "# 示例工作区\n\n当前任务：实现带 context 取消的 worker 池。\n约束：最多 4 个 worker；取消后等待全部 goroutine 退出。\n",
  readPrompt: "读取 note.md，说明当前任务和约束。",
  recoveryPrompt: "读取 note.md，失败后继续同一个任务。",
  careerKey: "career.target",
  firstGoal: "Go 后端开发实习",
  correctedGoal: "Agent 开发实习",
  memoryPrompt: "我保存的求职目标是什么？",
  interviewStart: "开始数据库模拟面试，只问一题。",
  reviewPrompt: "把本次面试的薄弱点和参考答案保存到 review.md。",
  interviewAnswer:
    "sql.DB 是连接池。SetMaxOpenConns 限制打开连接数，过小会让请求等待连接。",
  evidence: {
    id: "offline-sql-pool",
    source: "https://go.dev/doc/database/manage-connections",
    chunk_index: 0,
    distance: 0.95,
    text: "离线检索证据样例（非本次联网抓取）：sql.DB 管理数据库连接池。SetMaxOpenConns 限制打开的连接数；超过限制时请求会等待。持有连接并等待另一连接可能导致死锁。",
  },
};

export const scenes = [
  { id: "01-tool-execution" },
  { id: "02-personal-memory" },
  { id: "03-checkpoint-recovery" },
  { id: "04-interview" },
];

function json(res, body, status = 200) {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(body));
}
function answer(res, request, content, tool = null) {
  const finish = tool ? "tool_calls" : "stop";
  if (request.stream) {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    const delta = tool
      ? {
          tool_calls: [
            {
              index: 0,
              id: tool.id,
              type: "function",
              function: tool.function,
            },
          ],
        }
      : { content };
    res.end(
      `data: ${JSON.stringify({ choices: [{ index: 0, delta, finish_reason: finish }] })}\n\ndata: [DONE]\n\n`,
    );
  } else {
    json(res, {
      id: "offline-response",
      object: "chat.completion",
      model: "offline-demo",
      choices: [
        {
          index: 0,
          message: { role: "assistant", content },
          finish_reason: finish,
        },
      ],
      usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
    });
  }
}

export async function startFixtures() {
  const state = {
    mode: "tools",
    recoveryFailures: 0,
    toolRequests: 0,
    memoryRequests: [],
    interviews: [],
    requests: [],
    errors: [],
  };
  let base;
  const server = createServer(async (req, res) => {
    try {
      assert.equal(req.method, "POST", "unexpected fixture HTTP method");
      let raw = "";
      for await (const chunk of req) {
        raw += chunk;
        assert.ok(raw.length <= 1_048_576, "fixture request too large");
      }
      const body = JSON.parse(raw);
      state.requests.push({ mode: state.mode, route: req.url });
      if (req.url === "/v1/embeddings") {
        assert.equal(body.model, "text-embedding-v4");
        return json(res, {
          data: body.input.map((_, index) => ({ index, embedding: [1, 0] })),
        });
      }
      if (req.url === "/v2/vectordb/collections/describe") {
        assert.equal(body.collectionName, "offline_interview");
        return json(res, {
          code: 0,
          data: {
            description: `demo-rag-v1:${base}/v1:text-embedding-v4:2:raw-text`,
            fields: [
              {
                name: "vector",
                type: "FloatVector",
                params: [{ key: "dim", value: "2" }],
              },
            ],
          },
        });
      }
      if (req.url === "/v2/vectordb/entities/search") {
        assert.equal(body.filter, 'kb == "offline-interview"');
        assert.deepEqual(body.data, [[1, 0]]);
        return json(res, { code: 0, data: [fixture.evidence] });
      }
      assert.equal(req.url, "/v1/chat/completions", "unexpected fixture route");
      assert.equal(
        body.model,
        "offline-demo",
        "only the offline model may be used",
      );
      if (state.mode === "memory") {
        const memory = body.messages.find((m) =>
          m.content?.startsWith("[Current saved personal memory;"),
        );
        assert.ok(memory, "real CLI did not inject saved memory");
        const records = memory.content
          .split("\n")
          .slice(1)
          .filter(Boolean)
          .map((line) => JSON.parse(line));
        const fact = records.find((r) => r.key === fixture.careerKey);
        assert.ok(fact, "career memory missing");
        state.memoryRequests.push(fact);
        return answer(
          res,
          body,
          `你当前保存的求职目标是：${fact.content}。\n这条记忆来自 SQLite 当前版本 v${fact.version}。`,
        );
      }
      if (state.mode === "interview") {
        if (body.tools?.some((t) => t.function.name === "interview_start")) {
          const last = body.messages.at(-1);
          const call = (name, args) =>
            answer(res, body, "", {
              id: "offline-" + body.messages.length,
              function: { name, arguments: JSON.stringify(args) },
            });
          if (last.role !== "tool") {
            if (last.content === fixture.interviewStart)
              return call("interview_start", {
                topic: "database",
                questions: 1,
              });
            if (last.content === fixture.interviewAnswer)
              return call("interview_status", {});
            assert.equal(last.content, fixture.reviewPrompt);
            return call("review_list", {});
          }
          const toolCall = body.messages
            .flatMap((m) => m.tool_calls ?? [])
            .find((c) => c.id === last.tool_call_id);
          assert.ok(toolCall);
          const result = JSON.parse(last.content);
          switch (toolCall.function.name) {
            case "interview_start":
              return call("interview_next", {});
            case "interview_next":
              return answer(res, body, result.question.text);
            case "interview_status":
              assert.equal(result.answer_eligible, true);
              return call("interview_submit_answer", {
                question_id: result.question.id,
                input_ref: result.input_ref,
              });
            case "interview_submit_answer": {
              assert.equal(result.status, "reviewed");
              const e = result.evaluation;
              return answer(
                res,
                body,
                `参考评分：${e.score}/10\n点评：${e.feedback}\n参考回答：${e.reference_answer}\n来源：${result.sources.map((s) => s.source).join("，")}`,
              );
            }
            case "review_list": {
              assert.ok(result.weaknesses.length);
              const content =
                "# 面试复盘\n\n" +
                result.weaknesses
                  .map(
                    (w) =>
                      `## ${w.question}\n\n${w.feedback}\n\n参考要点：${w.reference_answer}\n\n${w.sources.join("\n")}`,
                  )
                  .join("\n");
              return call("write_file", {
                path: "review.md",
                content,
                expected_sha256: "",
              });
            }
            case "write_file":
              assert.equal(result.written, true);
              return answer(
                res,
                body,
                "已保存 review.md，包含本次薄弱点、参考要点和来源。",
              );
            default:
              throw new Error(
                "Unexpected interview tool " + toolCall.function.name,
              );
          }
        }

        const input = JSON.parse(body.messages.at(-1).content);
        assert.equal(input.candidate_answer, fixture.interviewAnswer);
        assert.equal(input.evidence[0].source, fixture.evidence.source);
        assert.ok(input.question.includes("连接池"));
        state.interviews.push(input);
        // Fixed assessment of this particular fixture, never a live model score.
        return answer(
          res,
          body,
          JSON.stringify({
            score: 7,
            feedback:
              "已说明连接池与连接上限；还需补充：持有连接并等待另一连接可能导致死锁。",
            references: [1],
            dimensions: { knowledge: 7, completeness: 6, expression: 8 },
          }),
        );
      }
      assert.ok(["tools", "recovery"].includes(state.mode));
      const observation = body.messages.findLast((m) => m.role === "tool");
      if (!observation) {
        const prompt = body.messages.at(-1).content;
        assert.equal(
          prompt,
          state.mode === "tools" ? fixture.readPrompt : fixture.recoveryPrompt,
        );
        assert.ok(body.tools.some((t) => t.function.name === "read_file"));
        state.toolRequests++;
        return answer(res, body, "", {
          id: "read-note",
          function: {
            name: "read_file",
            arguments: JSON.stringify({ path: fixture.file }),
          },
        });
      }
      const file = JSON.parse(observation.content);
      assert.equal(
        file.content,
        fixture.fileContent,
        "answer must be based on real read_file output",
      );
      assert.equal(file.path, fixture.file);
      if (state.mode === "recovery" && state.recoveryFailures === 0) {
        state.recoveryFailures++;
        return json(
          res,
          { error: { message: "offline injected failure" } },
          503,
        );
      }
      const text = file.content
        .split("\n")
        .filter((line) => /^(当前任务|约束)：/.test(line))
        .join("\n");
      assert.ok(text.includes("context"));
      answer(
        res,
        body,
        `${state.mode === "recovery" ? "已根据保存的工具结果继续。\n" : ""}${text}`,
      );
    } catch (error) {
      state.errors.push(error.message);
      json(
        res,
        { error: { message: "offline fixture assertion failed" } },
        500,
      );
    }
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  base = `http://127.0.0.1:${server.address().port}`;
  return {
    base,
    state,
    close: async () => {
      server.closeAllConnections();
      await new Promise((resolve, reject) =>
        server.close((err) => (err ? reject(err) : resolve())),
      );
    },
  };
}
