// npm run demo:images — real CLI scenarios, isolated local service fixtures.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import {
  mkdtemp,
  mkdir,
  readFile,
  writeFile,
  rm,
  copyFile,
  access,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { fixture, scenes, startFixtures } from "./demo-image-fixtures.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const runDir = path.join(root, ".runtime/demo-images");
const imageDir = path.join(root, "docs/images/demos");
const abort = new AbortController();
for (const signal of ["SIGINT", "SIGTERM"])
  process.once(signal, () => abort.abort());

function exec(
  command,
  args,
  { cwd = root, env, input = "", expected = 0 } = {},
) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd,
      env,
      signal: abort.signal,
      stdio: ["pipe", "pipe", "pipe"],
    });
    let stdout = "",
      stderr = "",
      size = 0;
    const timer = setTimeout(() => child.kill("SIGTERM"), 180_000);
    const collect = (name) => (chunk) => {
      size += chunk.length;
      if (size > 2_097_152) {
        child.kill("SIGTERM");
        return;
      }
      if (name === "stdout") stdout += chunk;
      else stderr += chunk;
    };
    child.stdout.on("data", collect("stdout"));
    child.stderr.on("data", collect("stderr"));
    child.on("error", (error) => {
      clearTimeout(timer);
      reject(error);
    });
    child.stdin.on("error", (error) => {
      if (error.code !== "EPIPE") reject(error);
    });
    child.once("close", (code, signal) => {
      clearTimeout(timer);
      if (code !== expected || size > 2_097_152)
        return reject(
          new Error(
            `${path.basename(command)} failed (exit=${code}, signal=${signal})\n${stderr}`,
          ),
        );
      resolve({ stdout, stderr, exitCode: code });
    });
    child.stdin.end(input);
  });
}
const readJSON = async (p) => JSON.parse(await readFile(p, "utf8"));
const readEvents = async (p) =>
  (await readFile(p, "utf8"))
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
const toolResults = (events) =>
  events.filter((e) => e.event.Type === "tool_result");

function escapeHTML(value) {
  return String(value).replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
}
function render(scene, template) {
  const blocks = scene.blocks
    .map(
      (block) =>
        `<section class="block"><div class="command"><span class="prompt">${block.kind === "input" ? "↳" : "$"}</span> ${escapeHTML(block.command)}</div><pre class="${block.error ? "error" : ""}">${escapeHTML(block.output.trim())}</pre></section>`,
    )
    .join("");
  return template.replace("{{blocks}}", () => blocks);
}

async function generate() {
  await mkdir(runDir, { recursive: true });
  await rm(path.join(runDir, "report.json"), { force: true });
  const temporary = await mkdtemp(path.join(tmpdir(), "lanagent-demo-images-"));
  let services, browser;
  const logs = [],
    outputs = [];
  const report = {
    mode: "offline-fixtures",
    success: false,
    scenes: [],
    normalization:
      "Temporary paths, identifiers and timestamps only; JSON previews project key fields.",
  };
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      ([key]) =>
        !/(API_KEY|TOKEN|PASSWORD|SECRET|PROXY|DATABASE_DSN|^LLM_|^EMBEDDING_|^MILVUS_|^MEMORY_)/i.test(
          key,
        ),
    ),
  );
  // Do not use personal Go workspace replacements or injected runtime options.
  Object.assign(env, {
    GOWORK: "off",
    LANAGENT_API_CONFIG: path.join(temporary, "missing-api.local.json"),
    CGO_ENABLED: "0",
    LLM_API_KEY: "offline-demo-not-a-secret",
    LLM_MODEL: "offline-demo",
    EMBEDDING_API_KEY: "offline-demo-not-a-secret",
    DATABASE_DSN: "",
    MEMORY_TOKEN: "",
    MILVUS_TOKEN: "",
  });
  const identifiers = new Map();
  function normalize(value) {
    return String(value)
      .replaceAll(temporary, "<临时目录>")
      .replaceAll(root.replace(/\/$/, ""), "<项目目录>")
      .replace(/\b[0-9a-f]{64}\b/g, (id) => {
        if (!identifiers.has(id))
          identifiers.set(id, `<SHA256-${identifiers.size + 1}>`);
        return identifiers.get(id);
      })
      .replace(/\b[0-9]{13}\b/g, "<时间戳>")
      .replace(/\b\d{4}-\d\d-\d\dT[\d:.]+Z\b/g, "<时间>");
  }
  function normalized(value) {
    if (typeof value === "string") return normalize(value);
    if (Array.isArray(value)) return value.map(normalized);
    if (value && typeof value === "object")
      return Object.fromEntries(
        Object.entries(value).map(([key, item]) => [key, normalized(item)]),
      );
    return value;
  }
  try {
    // Fail clearly if npm/browser setup is missing; never silently skip rendering.
    const { chromium } = await import("@playwright/test");
    const executablePath = process.env.CHROMIUM_PATH;
    await access(executablePath || chromium.executablePath()).catch(() => {
      throw new Error(
        "Chromium 未安装。运行 npx playwright install chromium，或设置 CHROMIUM_PATH。",
      );
    });
    console.log("Building isolated Go CLI binaries…");
    const harness = path.join(
      temporary,
      process.platform === "win32" ? "harness.exe" : "harness",
    );
    const interview = path.join(
      temporary,
      process.platform === "win32" ? "interview.exe" : "interview",
    );
    for (const [binary, pkg] of [
      [harness, "./cmd/harness"],
      [interview, "./cmd/interview"],
    ]) {
      await exec("go", ["build", "-o", binary, pkg], {
        cwd: root,
        env,
      });
    }
    services = await startFixtures();
    env.LLM_BASE_URL = services.base + "/v1";
    const work = path.join(temporary, "workspace"),
      config = path.join(temporary, "config");
    const states = path.join(temporary, "states");
    await mkdir(work);
    await mkdir(config);
    await mkdir(states);
    await writeFile(path.join(work, fixture.file), fixture.fileContent);
    const basicConfig = `merchant:\n  enabled: false\nbuilder:\n  enabled: false\nrag:\n  enabled: false\nmemory:\n  enabled: false\ntelemetry:\n  enabled: false\nllm:\n  base_url: ${JSON.stringify(services.base + "/v1")}\n  model: offline-demo\n  timeout: 10s\n  concurrency: 1\n`;
    await writeFile(path.join(config, "config.yaml"), basicConfig);
    async function cli(
      name,
      args,
      { state = name, expected = 0, input = "" } = {},
    ) {
      const result = await exec(
        harness,
        [
          "-workspace",
          work,
          "-config",
          config,
          "-state-dir",
          path.join(states, state),
          ...args,
        ],
        { env, expected, input },
      );
      logs.push({ name, command: ["harness", ...args], ...result });
      return result;
    }
    function block(args, result, { error = false, json = false } = {}) {
      let output = `${result.stderr}${result.stdout}`;
      if (json) {
        const r = JSON.parse(result.stdout);
        output = JSON.stringify(
          {
            scope: r.scope,
            key: r.key,
            content: r.content,
            version: r.version,
          },
          null,
          0,
        );
      }
      return {
        command:
          "harness " +
          args
            .map((a) => (/[\s？。]/.test(a) ? JSON.stringify(a) : a))
            .join(" "),
        output: normalize(output),
        error,
      };
    }

    console.log("Running four real CLI scenarios against loopback fixtures…");
    services.state.mode = "tools";
    let args = ["-session", "tools", "-prompt", fixture.readPrompt];
    const tools = await cli("tools", args);
    const toolsEvents = await readEvents(
      path.join(states, "tools/tools.jsonl"),
    );
    assert.equal(toolResults(toolsEvents).length, 1);
    assert.equal(
      JSON.parse(toolResults(toolsEvents)[0].event.Data.result).content,
      fixture.fileContent,
    );
    assert.ok(tools.stdout.includes("最多 4 个 worker"));
    assert.equal(services.state.toolRequests, 1);
    outputs.push({
      ...scenes[0],
      blocks: [block(args, tools)],
      checks: [
        "read_file 实际读取示例文件",
        "回答包含真实文件中的任务与约束",
        "JSONL 保存了 1 次工具调用与结果",
      ],
    });

    services.state.mode = "memory";
    const memoryBlocks = [];
    for (const [version, goal, session] of [
      [1, fixture.firstGoal, "memory-a"],
      [2, fixture.correctedGoal, "memory-b"],
    ]) {
      args = ["-prompt", `/remember user ${fixture.careerKey} ${goal}`];
      const saved = await cli(`memory-save-${version}`, args, {
        state: "memory",
      });
      const row = JSON.parse(saved.stdout);
      assert.equal(row.version, version);
      assert.equal(row.content, goal);
      memoryBlocks.push(block(args, saved, { json: true }));
      args = ["-session", session, "-prompt", fixture.memoryPrompt];
      const recalled = await cli(`memory-recall-${version}`, args, {
        state: "memory",
      });
      assert.ok(recalled.stdout.includes(goal));
      assert.equal(services.state.memoryRequests.at(-1).version, version);
      const snapshot = await readJSON(
        path.join(states, "memory", session + ".json"),
      );
      assert.equal(
        snapshot.messages.filter((m) => m.role === "user").length,
        1,
        "must be a new session",
      );
      if (version === 2) {
        assert.ok(!recalled.stdout.includes(fixture.firstGoal));
        assert.ok(
          !snapshot.messages.some((m) =>
            m.content.includes("[Current saved personal memory;"),
          ),
        );
      }
      memoryBlocks.push(block(args, recalled));
    }
    outputs.push({
      ...scenes[1],
      blocks: memoryBlocks,
      checks: [
        "两个独立 session 均收到 SQLite 当前记忆",
        "纠正后版本从 v1 增至 v2",
        "新会话 B 不再回答旧目标",
      ],
    });

    services.state.mode = "recovery";
    args = ["-session", "recover", "-prompt", fixture.recoveryPrompt];
    const failed = await cli("recovery-first", args, {
      state: "recovery",
      expected: 1,
    });
    assert.ok(failed.stderr.includes("503"));
    const pending = await readJSON(
      path.join(states, "recovery/recover.pending.json"),
    );
    assert.equal(pending.checkpoint.phase, "model");
    assert.equal(pending.checkpoint.running ?? "", "");
    assert.equal(
      pending.checkpoint.messages.filter((m) => m.role === "tool").length,
      1,
    );
    const before = toolResults(
      await readEvents(path.join(states, "recovery/recover.jsonl")),
    ).length;
    const resumeArgs = ["-session", "recover", "-resume"];
    const resumed = await cli("recovery-resumed", resumeArgs, {
      state: "recovery",
    });
    const after = toolResults(
      await readEvents(path.join(states, "recovery/recover.jsonl")),
    ).length;
    assert.equal(before, 1);
    assert.equal(after, before);
    assert.equal(
      services.state.toolRequests,
      2,
      "resume must not ask for read_file again",
    );
    assert.ok(resumed.stdout.includes("根据保存的工具结果继续"));
    assert.ok(
      (await readJSON(path.join(states, "recovery/recover.json"))).last_run,
    );
    await assert.rejects(
      access(path.join(states, "recovery/recover.pending.json")),
    );
    outputs.push({
      ...scenes[2],
      blocks: [
        block(args, failed, { error: true }),
        block(resumeArgs, resumed),
      ],
      checks: [
        `故障前 / 恢复后工具完成次数：${before} / ${after}`,
        "恢复使用已保存的工具观察，没有重放读取",
        "最终会话已提交，pending 检查点已清理",
      ],
    });

    services.state.mode = "interview";
    const interviewConfig = path.join(temporary, "interview-config");
    await mkdir(interviewConfig);
    await writeFile(
      path.join(interviewConfig, "config.yaml"),
      basicConfig.replace("rag:\n  enabled: false\n", "") +
        `\nembedding:\n  base_url: ${JSON.stringify(services.base + "/v1")}\n  model: text-embedding-v4\n  dimensions: 2\nrag:\n  enabled: true\n  milvus_url: ${JSON.stringify(services.base)}\n  collection: offline_interview\n  knowledge_base: offline-interview\n`,
    );
    const interviewBlocks = [];
    for (const [name, prompt] of [
      ["interview-start", fixture.interviewStart],
      ["interview-answer", fixture.interviewAnswer],
      ["interview-review", fixture.reviewPrompt],
    ]) {
      const args = [
        "-config",
        interviewConfig,
        "-session",
        "interview",
        "-allow-write",
        "-prompt",
        prompt,
      ];
      const result = await cli(name, args, { state: "interview" });
      interviewBlocks.push(
        block(
          ["-session", "interview", "-allow-write", "-prompt", prompt],
          result,
        ),
      );
    }
    const savedInterview = await readJSON(
      path.join(states, "interview/interview.interview.json"),
    );
    const training = savedInterview.trainings[savedInterview.active].coach;
    assert.equal(training.history.length, 1);
    assert.equal(training.history[0].answer, fixture.interviewAnswer);
    assert.equal(training.history[0].evaluation.score, 7);
    assert.deepEqual(training.history[0].evaluation.references, [1]);
    assert.ok(
      training.history[0].evaluation.reference_answer.includes(
        "SetMaxOpenConns",
      ),
    );
    const review = await readFile(path.join(work, "review.md"), "utf8");
    assert.ok(review.includes(fixture.evidence.source));
    assert.ok(review.includes("连接池"));
    assert.equal(services.state.interviews.length, 1);
    outputs.push({
      ...scenes[3],
      blocks: interviewBlocks,
      checks: [
        "统一 CLI 使用原始用户输入进行点评",
        "参考答案由程序维护",
        "review_list 读取弱项，write_file 实际保存复盘",
      ],
    });

    assert.deepEqual(services.state.errors, []);
    for (const scene of outputs) {
      // No hidden truncation: screenshots wrap the actual selected output.
      for (const b of scene.blocks) {
        assert.ok(!b.output.includes(temporary));
        assert.ok(!b.output.includes("offline-demo-not-a-secret"));
      }
    }
    const template = await readFile(
      new URL("./demo-terminal.html", import.meta.url),
      "utf8",
    );
    browser = await chromium.launch({
      headless: true,
      ...(executablePath ? { executablePath } : {}),
    });
    const context = await browser.newContext({
      viewport: { width: 1440, height: 100 },
      deviceScaleFactor: 1,
      locale: "zh-CN",
      colorScheme: "light",
    });
    const page = await context.newPage();
    // Rendering is entirely local. A template cannot silently load remote assets.
    await page.route("**/*", (route) => route.abort());
    for (const scene of outputs) {
      const html = render(scene, template);
      await writeFile(path.join(runDir, scene.id + ".html"), html);
      await page.setContent(html, { waitUntil: "load" });
      await page.evaluate(() => document.fonts.ready);
      const geometry = await page.evaluate(() => ({
        width: document.documentElement.scrollWidth,
        height: document.documentElement.scrollHeight,
        overflow: [...document.querySelectorAll("pre,.command")].some(
          (e) => e.scrollWidth > e.clientWidth + 1,
        ),
      }));
      assert.equal(geometry.width, 1440, `${scene.id}: horizontal overflow`);
      assert.equal(geometry.overflow, false, `${scene.id}: clipped output`);
      assert.ok(
        geometry.height < 2000,
        `${scene.id}: image too tall; edit the scene, do not silently crop it`,
      );
      await page.screenshot({
        path: path.join(runDir, scene.id + ".png"),
        fullPage: true,
        animations: "disabled",
      });
      report.scenes.push({ id: scene.id, checks: scene.checks, ...geometry });
    }
    await context.close();
    // Publish only after every behavior assertion and all four renders passed.
    await mkdir(imageDir, { recursive: true });
    for (const scene of scenes)
      await copyFile(
        path.join(runDir, scene.id + ".png"),
        path.join(imageDir, scene.id + "-terminal.png"),
      );
    report.success = true;
    console.log(
      `PASS: ${scenes.length} validated screenshots → docs/images/demos/`,
    );
  } catch (error) {
    report.error = normalize(error.message);
    report.stack = normalize(error.stack);
    throw error;
  } finally {
    if (browser) await browser.close();
    if (services) await services.close();
    await writeFile(
      path.join(runDir, "transcripts.json"),
      JSON.stringify(normalized(logs), null, 2),
    );
    if (services)
      await writeFile(
        path.join(runDir, "fixtures.json"),
        JSON.stringify(services.state, null, 2),
      );
    await rm(temporary, { recursive: true, force: true });
    await writeFile(
      path.join(runDir, "report.json"),
      JSON.stringify(report, null, 2) + "\n",
    );
  }
}

await generate().catch((error) => {
  console.error(error.message);
  process.exitCode = 1;
});
