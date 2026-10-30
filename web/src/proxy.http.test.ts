import { spawn } from "node:child_process";
import { access } from "node:fs/promises";
import { request, type IncomingHttpHeaders } from "node:http";
import { createRequire } from "node:module";
import { createConnection, createServer } from "node:net";
import { fileURLToPath } from "node:url";
import { formatChildFailure } from "./lib/redact";
import { describe, expect, it, type TestContext } from "vitest";

const webRoot = fileURLToPath(new URL("../", import.meta.url));
const nextCLI = createRequire(import.meta.url).resolve("next/dist/bin/next");
const paths = [
  "/api/dashboard",
  "/api/history",
  "/api/money-history",
  "/api/standings-history",
];

type Response = {
  status: number;
  headers: IncomingHttpHeaders;
  body: string;
};

function expectDenied(response: Response) {
  expect(response.status).toBe(403);
  expect(response.headers["content-type"]).toBe("text/plain; charset=utf-8");
  expect(response.body).toBe("Forbidden host\n");
}

async function scratchPort() {
  const server = createServer();
  const listening = Promise.withResolvers<void>();
  server.once("error", listening.reject);
  server.listen(0, "127.0.0.1", listening.resolve);
  await listening.promise;
  const address = server.address();
  const closed = Promise.withResolvers<void>();
  server.close((error) => (error ? closed.reject(error) : closed.resolve()));
  await closed.promise;
  if (!address || typeof address === "string") {
    throw new Error("Scratch server did not receive a TCP port");
  }
  return address.port;
}

async function runMode(context: TestContext, mode: "dev" | "start") {
  if (mode === "start") {
    try {
      await access(new URL("../.next/BUILD_ID", import.meta.url));
    } catch (error) {
      if (!(
        error instanceof Error &&
        "code" in error &&
        error.code === "ENOENT"
      )) {
        throw error;
      }
      context.skip(
        "Production HTTP host boundary skipped: web/.next/BUILD_ID is missing; run pnpm --dir web build",
      );
    }
  }

  const port = await scratchPort();
  const child = spawn(
    process.execPath,
    [nextCLI, mode, "--hostname", "127.0.0.1", "--port", String(port)],
    {
      cwd: webRoot,
      shell: false,
      windowsHide: true,
      detached: process.platform !== "win32",
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        NODE_ENV: mode === "dev" ? "development" : "production",
        DASHBOARD_ALLOWED_HOSTS: "",
        CI: "1",
        NEXT_TELEMETRY_DISABLED: "1",
      },
    },
  );
  let stopping = false;
  let childClosed = false;
  let stdout = "";
  let stderr = "";
  const { promise: failed, reject: fail } = Promise.withResolvers<never>();
  void failed.catch(() => {});
  child.once("error", fail);
  child.once("exit", (code, signal) => {
    if (!stopping) {
      fail(new Error(`Next ${mode} exited (${code ?? signal})`));
    }
  });
  const { promise: closed, resolve: markClosed } =
    Promise.withResolvers<void>();
  child.once("close", () => {
    childClosed = true;
    markClosed();
  });
  // Accumulate raw output without truncation so a URL split across chunk
  // boundaries is still redacted whole; the failure message slices after
  // redaction.
  child.stdout.on("data", (chunk: Buffer) => {
    stdout += chunk.toString();
  });
  child.stderr.on("data", (chunk: Buffer) => {
    stderr += chunk.toString();
  });

  // Real child processes and sockets require platform-clock deadlines, not fake timers.
  const pending = new Set<() => void>();
  let cleanupPromise: Promise<void> | undefined;
  function cleanup() {
    cleanupPromise ??= (async () => {
      stopping = true;
      for (const cancel of pending) {
        cancel();
      }
      const timeout = Promise.withResolvers<never>();
      const deadline = setTimeout(() => {
        child.kill("SIGKILL");
        timeout.reject(new Error(`Next ${mode} cleanup exceeded 5,000 ms`));
      }, 5_000);
      let escalation: NodeJS.Timeout | undefined;
      try {
        await Promise.race([
          (async () => {
            if (child.pid && !childClosed) {
              if (process.platform === "win32") {
                try {
                  const killed = Promise.withResolvers<void>();
                  const killer = spawn(
                    "taskkill.exe",
                    ["/PID", String(child.pid), "/T", "/F"],
                    { shell: false, windowsHide: true, stdio: "ignore" },
                  );
                  killer.once("error", killed.reject);
                  killer.once("close", (code) => {
                    if (code === 0) {
                      killed.resolve();
                    } else {
                      killed.reject(
                        new Error(`taskkill exited with code ${code}`),
                      );
                    }
                  });
                  await killed.promise;
                } finally {
                  if (!childClosed) {
                    child.kill("SIGKILL");
                  }
                  await closed;
                }
              } else {
                const killGroup = (signal: NodeJS.Signals) => {
                  try {
                    process.kill(-child.pid!, signal);
                  } catch (error) {
                    if (!(
                      error instanceof Error &&
                      "code" in error &&
                      error.code === "ESRCH"
                    )) {
                      throw error;
                    }
                  }
                };
                killGroup("SIGTERM");
                escalation = setTimeout(() => {
                  try {
                    killGroup("SIGKILL");
                  } catch (error) {
                    timeout.reject(error);
                  }
                }, 2_000);
                await closed;
              }
            } else {
              await closed;
            }
          })(),
          timeout.promise,
        ]);
      } finally {
        clearTimeout(deadline);
        clearTimeout(escalation);
        context.signal.removeEventListener("abort", onAbort);
      }
    })();
    return cleanupPromise;
  }
  function onAbort() {
    fail(new Error(`Next ${mode} HTTP test canceled`));
    void cleanup().catch(() => {});
  }
  context.signal.addEventListener("abort", onAbort, { once: true });
  context.onTestFinished(cleanup, 5_000);

  function httpRequest(path: string, host: string, deadline: number) {
    const { promise, resolve, reject } = Promise.withResolvers<Response>();
    const req = request(
      {
        hostname: "127.0.0.1",
        port,
        path,
        method: "GET",
        agent: false,
        headers: {
          Host: host,
          Connection: "close",
          "X-Forwarded-Host": "localhost:3400",
        },
      },
      (response) => {
        let body = "";
        response.setEncoding("utf8");
        response.on("data", (chunk: string) => {
          body += chunk;
        });
        response.once("error", finish);
        response.once("end", () => {
          finish(undefined, {
            status: response.statusCode!,
            headers: response.headers,
            body,
          });
        });
      },
    );
    const cancel = () => {
      clearTimeout(timer);
      req.destroy(new Error(`HTTP request canceled: ${path}`));
    };
    const timer = setTimeout(
      () => {
        req.destroy(new Error(`HTTP request deadline exceeded: ${path}`));
      },
      Math.max(0, deadline - Date.now()),
    );
    pending.add(cancel);
    function finish(error?: Error, response?: Response) {
      clearTimeout(timer);
      pending.delete(cancel);
      req.destroy();
      if (error) {
        reject(error);
      } else {
        resolve(response!);
      }
    }
    req.once("error", finish);
    req.end();
    return promise;
  }

  function missingHostRequest(path: string, deadline: number) {
    const { promise, resolve, reject } = Promise.withResolvers<Response>();
    const socket = createConnection({ host: "127.0.0.1", port });
    let raw = "";
    socket.setEncoding("utf8");
    const cancel = () => {
      clearTimeout(timer);
      socket.destroy(new Error(`Missing-Host request canceled: ${path}`));
    };
    const timer = setTimeout(
      () => {
        socket.destroy(
          new Error(`Missing-Host request deadline exceeded: ${path}`),
        );
      },
      Math.max(0, deadline - Date.now()),
    );
    pending.add(cancel);
    socket.once("connect", () => {
      // HTTP/1.1 without Host is rejected by Node before proxy can run.
      socket.write(
        `GET ${path} HTTP/1.0\r\nConnection: close\r\nX-Forwarded-Host: localhost:3400\r\n\r\n`,
      );
    });
    socket.on("data", (chunk: string) => {
      raw += chunk;
    });
    socket.once("error", reject);
    socket.once("close", (hadError) => {
      clearTimeout(timer);
      pending.delete(cancel);
      if (hadError) {
        return;
      }
      const split = raw.indexOf("\r\n\r\n");
      const lines = raw.slice(0, split).split("\r\n");
      const status = /^HTTP\/1\.[01] (\d{3})\b/.exec(lines.shift() ?? "");
      if (split < 0 || !status) {
        reject(new Error(`Incomplete HTTP response for missing Host: ${raw}`));
        return;
      }
      const headers: IncomingHttpHeaders = {};
      for (const line of lines) {
        const colon = line.indexOf(":");
        if (colon < 1) {
          reject(new Error(`Malformed HTTP response header: ${line}`));
          return;
        }
        headers[line.slice(0, colon).toLowerCase()] = line
          .slice(colon + 1)
          .trim();
      }
      resolve({
        status: Number(status[1]),
        headers,
        body: raw.slice(split + 4),
      });
    });
    return promise;
  }

  try {
    context.signal.throwIfAborted();
    const readyDeadline = Date.now() + (mode === "dev" ? 20_000 : 10_000);
    while (true) {
      try {
        expectDenied(
          await Promise.race([
            httpRequest(
              "/api/dashboard",
              "foreign.example:3400",
              readyDeadline,
            ),
            failed,
          ]),
        );
        break;
      } catch (error) {
        if (!(
          error instanceof Error &&
          "code" in error &&
          error.code === "ECONNREFUSED"
        )) {
          throw error;
        }
        if (Date.now() >= readyDeadline) {
          throw new Error(`Next ${mode} startup deadline exceeded`, {
            cause: error,
          });
        }
        const delay = Promise.withResolvers<void>();
        const cancel = () => {
          clearTimeout(timer);
          pending.delete(cancel);
          delay.reject(new Error("Readiness polling canceled"));
        };
        const timer = setTimeout(
          () => {
            pending.delete(cancel);
            delay.resolve();
          },
          Math.min(100, readyDeadline - Date.now()),
        );
        pending.add(cancel);
        await Promise.race([delay.promise, failed]);
      }
    }

    const matrixDeadline = Date.now() + 5_000;
    await Promise.race([
      Promise.all(
        paths.flatMap((path) => [
          httpRequest(path, "foreign.example:3400", matrixDeadline).then(
            expectDenied,
          ),
          missingHostRequest(path, matrixDeadline).then(expectDenied),
          httpRequest(path, "localhost:3400", matrixDeadline).then(
            (response) => {
              expect(response.status).not.toBe(403);
            },
          ),
        ]),
      ),
      failed,
    ]);
  } catch (error) {
    throw new Error(formatChildFailure(mode, stdout, stderr), {
      cause: error,
    });
  } finally {
    await cleanup();
  }
}

describe(
  "dashboard HTTP host boundary",
  { concurrent: false, shuffle: false },
  () => {
    it("development", { timeout: 35_000, retry: 0 }, async (context) => {
      await runMode(context, "dev");
    });

    it("production", { timeout: 25_000, retry: 0 }, async (context) => {
      await runMode(context, "start");
    });
  },
);
