import crypto from "node:crypto";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import readline from "node:readline";
import { spawn } from "node:child_process";

const RESPONSE_PREFIX = "__MCPX_BROWSER_RESPONSE__";
const TRUSTED_SERVICE = "mcpxBrowserService";
const RPC_TIMEOUT_MS = 130_000;
const JS_TIMEOUT_MS = 120_000;

const servicePath = process.env.MCPX_BROWSER_SERVICE_PATH?.trim();
const nodeReplPath = process.env.MCPX_NODE_REPL_PATH?.trim();
const nodePath = process.env.MCPX_NODE_PATH?.trim();
const codexCliPath = process.env.MCPX_CODEX_CLI_PATH?.trim();
if (!servicePath || !nodeReplPath || !nodePath || !codexCliPath) {
  throw new Error("MCPX browser service host paths are required");
}

function parseNodeReplArgs() {
  const raw = process.env.MCPX_NODE_REPL_ARGS?.trim();
  if (!raw) return [];
  const parsed = JSON.parse(raw);
  if (!Array.isArray(parsed) || !parsed.every((value) => typeof value === "string")) {
    throw new Error("MCPX_NODE_REPL_ARGS must be a JSON string array");
  }
  return parsed;
}

function trustedServicesEnv() {
  let services = {};
  const raw = process.env.NODE_REPL_TRUSTED_SERVICES?.trim();
  if (raw) {
    try {
      const parsed = JSON.parse(raw);
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) services = parsed;
    } catch {
      // Ignore unrelated malformed inherited configuration and install the one
      // service MCPX requires below.
    }
  }
  return JSON.stringify({ ...services, [TRUSTED_SERVICE]: servicePath });
}

function trustedCodePathsEnv() {
  const values = new Set([path.dirname(servicePath)]);
  for (const value of (process.env.NODE_REPL_TRUSTED_CODE_PATHS ?? "").split(path.delimiter)) {
    if (value.trim()) values.add(value.trim());
  }
  return [...values].join(path.delimiter);
}

const childEnv = {
  ...process.env,
  CODEX_CLI_PATH: codexCliPath,
  CODEX_HOME: process.env.CODEX_HOME?.trim() || path.join(os.homedir(), ".codex"),
  NODE_REPL_NODE_PATH: nodePath,
  NODE_REPL_TRUSTED_RPC_ENABLED: "1",
  NODE_REPL_TRUSTED_SERVICES: trustedServicesEnv(),
  NODE_REPL_TRUSTED_CODE_PATHS: trustedCodePathsEnv(),
  BROWSER_USE_AVAILABLE_BACKENDS: "chrome",
  BROWSER_USE_TINYSKY_ENABLED: "0",
};

const nodeReplProcess = spawn(nodeReplPath, parseNodeReplArgs(), {
  env: childEnv,
  stdio: ["pipe", "pipe", "pipe"],
  windowsHide: true,
});

nodeReplProcess.stderr.on("data", (chunk) => process.stderr.write(chunk));

let rpcNextId = 1;
let childExitError = null;
let activeRequest = null;
const rpcPending = new Map();

function sendRpc(message) {
  if (childExitError) throw childExitError;
  nodeReplProcess.stdin.write(`${JSON.stringify(message)}\n`);
}

function rpcRequest(method, params, timeoutMs = RPC_TIMEOUT_MS) {
  if (childExitError) return Promise.reject(childExitError);
  const id = rpcNextId++;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      if (!rpcPending.delete(id)) return;
      reject(new Error(`OpenAI node_repl request timed out: ${method}`));
    }, timeoutMs);
    rpcPending.set(id, { reject, resolve, timer });
    try {
      sendRpc({ jsonrpc: "2.0", id, method, params });
    } catch (error) {
      clearTimeout(timer);
      rpcPending.delete(id);
      reject(error);
    }
  });
}

function sanitizedElicitationMeta(params) {
  const source = params?._meta;
  if (!source || typeof source !== "object" || Array.isArray(source)) return null;
  const meta = { ...source };
  delete meta.progressToken;
  delete meta["x-codex-turn-metadata"];
  return Object.keys(meta).length > 0 ? meta : null;
}

function sanitizedResponseMeta(source, commandType) {
  if (!source || typeof source !== "object" || Array.isArray(source)) return null;
  const meta = { ...source };
  if (commandType === "tab_screenshot") return meta;

  const surface = meta["codex/toolSurface"];
  if (!surface || typeof surface !== "object" || Array.isArray(surface)) return meta;
  const screenshot = surface.screenshot;
  if (!screenshot || typeof screenshot !== "object" || Array.isArray(screenshot)) return meta;
  if (typeof screenshot.url !== "string" || !screenshot.url.startsWith("data:image/")) return meta;

  const sanitizedScreenshot = { ...screenshot };
  delete sanitizedScreenshot.url;
  const sanitizedSurface = { ...surface };
  if (Object.keys(sanitizedScreenshot).length > 0) sanitizedSurface.screenshot = sanitizedScreenshot;
  else delete sanitizedSurface.screenshot;
  meta["codex/toolSurface"] = sanitizedSurface;
  return meta;
}

function fingerprintElicitation(message, meta) {
  return crypto.createHash("sha256").update(JSON.stringify({ message, meta })).digest("hex");
}

function handleElicitation(message) {
  const params = message.params ?? {};
  const prompt = typeof params.message === "string" ? params.message : "Browser permission required";
  const meta = sanitizedElicitationMeta(params);
  const fingerprint = fingerprintElicitation(prompt, meta);
  const approved = activeRequest?.approvedFingerprints.has(fingerprint) === true;
  if (approved) {
    const persist = Array.isArray(meta?.persist) && meta.persist.includes("session")
      ? { persist: "session" }
      : {};
    sendRpc({ jsonrpc: "2.0", id: message.id, result: { action: "accept", content: persist } });
    return;
  }
  if (activeRequest) {
    activeRequest.elicitations.push({ fingerprint, message: prompt, meta });
  }
  sendRpc({ jsonrpc: "2.0", id: message.id, result: { action: "decline" } });
}

function handleNodeReplMessage(message) {
  if (message?.method && message.id != null) {
    if (message.method === "elicitation/create") {
      handleElicitation(message);
    } else {
      sendRpc({
        jsonrpc: "2.0",
        id: message.id,
        error: { code: -32601, message: `Unsupported node_repl client request: ${message.method}` },
      });
    }
    return;
  }
  if (message?.method) return;
  const pending = rpcPending.get(message?.id);
  if (!pending) return;
  clearTimeout(pending.timer);
  rpcPending.delete(message.id);
  if (message.error) {
    pending.reject(new Error(message.error.message || "OpenAI node_repl request failed"));
  } else {
    pending.resolve(message.result);
  }
}

const childLines = readline.createInterface({ input: nodeReplProcess.stdout, crlfDelay: Infinity });
childLines.on("line", (line) => {
  if (!line.trim()) return;
  try {
    handleNodeReplMessage(JSON.parse(line));
  } catch (error) {
    process.stderr.write(`MCPX could not decode node_repl output: ${String(error)}\n`);
  }
});

nodeReplProcess.on("error", (error) => {
  childExitError = error;
  for (const pending of rpcPending.values()) {
    clearTimeout(pending.timer);
    pending.reject(error);
  }
  rpcPending.clear();
});

nodeReplProcess.on("exit", (code, signal) => {
  childExitError = new Error(`OpenAI node_repl exited unexpectedly (code=${String(code)}, signal=${String(signal)})`);
  for (const pending of rpcPending.values()) {
    clearTimeout(pending.timer);
    pending.reject(childExitError);
  }
  rpcPending.clear();
});

await rpcRequest("initialize", {
  protocolVersion: "2025-06-18",
  capabilities: { elicitation: { form: {} } },
  clientInfo: { name: "mcpx-browser-service", version: "0.9.10" },
});
sendRpc({ jsonrpc: "2.0", method: "notifications/initialized" });

function requestMetaFor(sessionId, turnId) {
  return {
    "x-codex-turn-metadata": JSON.stringify({
      session_id: sessionId,
      turn_id: turnId,
      thread_id: sessionId,
      thread_source: "user",
      model: "mcpx",
    }),
  };
}

function browserExecutionCode(request) {
  const encodedRequest = Buffer.from(JSON.stringify(request), "utf8").toString("base64");
  return `
await (async () => {
const encodedRequest = ${JSON.stringify(encodedRequest)};
const mcpxRequest = JSON.parse(Buffer.from(encodedRequest, "base64").toString("utf8"));
if (!globalThis.__mcpxBrowserSetupDone) {
  await nodeRepl.rpc(${JSON.stringify(TRUSTED_SERVICE)}, {
    method: "setup",
    params: { environment: "codex-app", undocumentedApiMembers: [], excludedDocumentation: [] },
  });
  globalThis.__mcpxBrowserSetupDone = true;
}
try {
  const command = { ...mcpxRequest.command };
  if (command.type !== "list_browsers") {
    const browsers = await nodeRepl.rpc(${JSON.stringify(TRUSTED_SERVICE)}, {
      method: "execute",
      params: { type: "list_browsers" },
    });
    const extensions = Array.isArray(browsers) ? browsers.filter((browser) => browser?.type === "extension") : [];
    if (extensions.length === 0) throw new Error("No OpenAI extension browser is available");
    const instanceId = String(mcpxRequest.browser_instance_id ?? "").trim();
    let browser;
    if (instanceId) {
      browser = extensions.find((candidate) => candidate?.metadata?.extensionInstanceId === instanceId);
      if (!browser) throw new Error(\`Browser extension instance is not available: \${instanceId}\`);
    } else {
      if (extensions.length !== 1) throw new Error("Multiple extension browsers are available; browser_instance_id is required");
      browser = extensions[0];
    }
    command.browser_id = browser.id;
  }
  const result = await nodeRepl.rpc(${JSON.stringify(TRUSTED_SERVICE)}, { method: "execute", params: command });
  nodeRepl.write(${JSON.stringify(RESPONSE_PREFIX)} + JSON.stringify({ ok: true, result: result ?? null }));
} catch (caught) {
  nodeRepl.write(${JSON.stringify(RESPONSE_PREFIX)} + JSON.stringify({
    ok: false,
    result: null,
    error: {
      name: caught instanceof Error ? caught.name : "Error",
      message: caught instanceof Error ? caught.message : String(caught),
    },
  }));
}
})();
`;
}

function textError(result) {
  const values = Array.isArray(result?.content)
    ? result.content.filter((item) => item?.type === "text" && typeof item.text === "string").map((item) => item.text)
    : [];
  return values.join("\n").trim() || "OpenAI node_repl JavaScript execution failed";
}

function parseBrowserToolResult(toolResult, commandType) {
  let envelope = null;
  const contentItems = [];
  for (const item of Array.isArray(toolResult?.content) ? toolResult.content : []) {
    if (item?.type === "text" && typeof item.text === "string" && item.text.startsWith(RESPONSE_PREFIX)) {
      envelope = JSON.parse(item.text.slice(RESPONSE_PREFIX.length));
    } else {
      contentItems.push(item);
    }
  }
  if (!envelope && toolResult?.isError) {
    return {
      ok: false,
      result: null,
      error: { name: "NodeReplError", message: textError(toolResult) },
      content_items: contentItems,
      response_meta: sanitizedResponseMeta(toolResult?._meta, commandType),
    };
  }
  if (!envelope) {
    const diagnostic = {
      keys: toolResult && typeof toolResult === "object" ? Object.keys(toolResult) : [],
      isError: toolResult?.isError === true,
      content: (Array.isArray(toolResult?.content) ? toolResult.content : []).map((item) => ({
        type: item?.type ?? null,
        textLength: typeof item?.text === "string" ? item.text.length : null,
        hasResponsePrefix: typeof item?.text === "string" && item.text.startsWith(RESPONSE_PREFIX),
      })),
      metaKeys: toolResult?._meta && typeof toolResult._meta === "object" ? Object.keys(toolResult._meta) : [],
    };
    throw new Error(`OpenAI node_repl did not return a Browser Service response: ${JSON.stringify(diagnostic)}`);
  }
  return {
    ...envelope,
    content_items: contentItems,
    response_meta: sanitizedResponseMeta(toolResult?._meta, commandType),
  };
}

async function executeRequest(request) {
  if (!request || typeof request !== "object" || Array.isArray(request)) throw new Error("Invalid sidecar request");
  if (!request.command || typeof request.command !== "object" || Array.isArray(request.command)) throw new Error("Browser command is required");
  const sessionId = String(request.session_id ?? "").trim();
  const turnId = String(request.turn_id ?? "").trim();
  if (!sessionId || !turnId) throw new Error("session_id and turn_id are required");

  activeRequest = {
    approvedFingerprints: new Set(Array.isArray(request.approved_fingerprints) ? request.approved_fingerprints : []),
    elicitations: [],
  };
  try {
    const toolResult = await rpcRequest("tools/call", {
      name: "js",
      arguments: {
        title: `Browser use: ${String(request.command.type ?? "command")}`,
        code: browserExecutionCode(request),
        timeout_ms: JS_TIMEOUT_MS,
      },
      _meta: requestMetaFor(sessionId, turnId),
    });
    const response = parseBrowserToolResult(toolResult, String(request.command.type ?? "").trim());
    return { ...response, elicitations: [...activeRequest.elicitations] };
  } finally {
    activeRequest = null;
  }
}

const protocolLines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
try {
  for await (const line of protocolLines) {
    if (!line.trim()) continue;
    let request;
    try {
      request = JSON.parse(line);
      const response = await executeRequest(request);
      process.stdout.write(`${JSON.stringify({ id: request.id, ...response })}\n`);
    } catch (caught) {
      process.stdout.write(`${JSON.stringify({
        id: request?.id ?? null,
        ok: false,
        result: null,
        error: {
          name: caught instanceof Error ? caught.name : "Error",
          message: caught instanceof Error ? caught.message : String(caught),
        },
        elicitations: activeRequest?.elicitations ?? [],
        content_items: [],
        response_meta: null,
      })}\n`);
      activeRequest = null;
    }
  }
} finally {
  childLines.close();
  nodeReplProcess.stdin.end();
  nodeReplProcess.kill();
}
