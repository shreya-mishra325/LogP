const {test, expect} = require("@playwright/test");
const {spawn, execFile} = require("node:child_process");
const {promisify} = require("node:util");
const {writeFile, readFile} = require("node:fs/promises");
const {once} = require("node:events");
const readline = require("node:readline");
const path = require("node:path");

const operator = "test-only-operator";
const worker = "test-only-worker";
const example = {
  kernel: "mandelbrot.v1", width: 65, height: 49, max_iter: 32,
  xmin: -2.2, xmax: 0.8, ymin: -1.2, ymax: 1.2, tile_size: 16,
};

const binary = process.env.COORDINATOR_BINARY ||
  path.resolve(__dirname, "..", process.platform === "win32" ? "logp.exe" : "logp");
let child, origin;

async function cli(command, ...args) {
  const {stdout} = await promisify(execFile)(binary, [command, "--server", origin, ...args], {
    timeout: 10000,
    env: {...process.env, BROWSER_COMPUTE_OPERATOR_TOKEN: operator},
  });
  return stdout ? JSON.parse(stdout) : undefined;
}
test.beforeAll(async () => {
  child = spawn(binary, ["serve", "--listen", "127.0.0.1:0"], {
    env: {...process.env, BROWSER_COMPUTE_OPERATOR_TOKEN: operator, BROWSER_COMPUTE_WORKER_TOKEN: worker},
    stdio: ["ignore", "pipe", "inherit"],
  });
  const lines = readline.createInterface({input: child.stdout});
  const [line] = await Promise.race([
    once(lines, "line"),
    once(child, "exit").then(([code]) => { throw new Error("Coordinator exited: " + code); }),
  ]);
  expect(line).toMatch(/^Listening on 127\.0\.0\.1:\d+$/);
  origin = "http://" + line.slice("Listening on ".length);
});
test.afterAll(async () => {
  if (child?.pid && child.exitCode === null && child.signalCode === null) {
    const exited = once(child, "exit");
    child.kill("SIGTERM");
    const [status] = await exited;
    expect(status).toBe(0);
  }
});

async function openWorker(context, held = false) {
  await context.addInitScript(hold => {
    const Native = window.Worker;
    const NativeSocket = window.WebSocket;
    const gate = window.testGate = {started: 0, terminated: 0, sockets: 0, assignments: [], pending: [], hold};
    window.WebSocket = class extends NativeSocket {
      constructor(...args) {
        super(...args);
        gate.sockets++;
        this.addEventListener("message", ({data}) => {
          const message = JSON.parse(data);
          if (message.type === "assign") gate.assignments.push(message.payload);
        });
      }
    };
    window.Worker = class extends Native {
      constructor(...args) {
        super(...args);
        gate.started++;
      }
      postMessage(...args) {
        if (gate.hold) {
          this.release = () => super.postMessage(...args);
          gate.pending.push(this);
        } else super.postMessage(...args);
      }
      terminate() {
        gate.terminated++;
        this.release = null;
        super.terminate();
      }
    };
  }, held);
  const page = await context.newPage();
  await page.goto(origin + "/worker");
  return page;
}

async function join(page) {
  await page.getByLabel("Worker credential").fill(worker);
  await page.getByRole("button", {name: "Join", exact: true}).click();
  await expect(page.locator("#status")).toHaveText("Joined");
}

async function submit(api) {
  const response = await api.post("/api/v1/jobs", {data: example});
  expect(response.status()).toBe(201);
  return "/api/v1/jobs/" + (await response.json()).id;
}

async function operatorAPI(playwright) {
  return playwright.request.newContext({baseURL: origin, extraHTTPHeaders: {Authorization: "Bearer " + operator}});
}

function reference() {
  const {width, height, xmin, xmax, ymin, ymax, max_iter} = example;
  const iter = Buffer.alloc(2 * width * height);
  const rgba = Buffer.alloc(4 * width * height);
  for (let index = 0; index < width * height; index++) {
    const x = xmin + (index % width + 0.5) * (xmax - xmin) / width;
    const y = ymax - (Math.floor(index / width) + 0.5) * (ymax - ymin) / height;
    let a = 0, b = 0, n = 0;
    while (n < max_iter && a * a + b * b <= 4) {
      [a, b] = [a * a - b * b + x, (2 * a) * b + y];
      n++;
    }
    iter.writeUInt16LE(n, index * 2);
    rgba.set(n === max_iter ? [0, 0, 0, 255] : [9 * n % 256, 5 * n % 256, 13 * n % 256, 255], index * 4);
  }
  return {iter, rgba};
}

test("two consenting Chromium contexts render tiles and recover a disconnected lease", async ({browser, playwright}, testInfo) => {
  const api = await operatorAPI(playwright);
  const first = await browser.newContext();
  const second = await browser.newContext();
  try {
    const a = await openWorker(first, true);
    const file = testInfo.outputPath("mandelbrot.json");
    await writeFile(file, JSON.stringify(example));
    const {id} = await cli("submit", "--file", file);
    const job = "/api/v1/jobs/" + id;
    expect(await a.evaluate(() => [testGate.sockets, testGate.started])).toEqual([0, 0]);
    expect((await cli("status", "--job", id)).state).toBe("QUEUED");
    await join(a);
    await expect(a.locator("#active")).toHaveText("1");
    await a.evaluate(() => testGate.pending[0].release());
    await expect.poll(async () => (await (await api.get(job)).json()).accepted).toBe(1);
    await expect.poll(() => a.evaluate(() => testGate.assignments.length)).toBe(2);
    const abandoned = await a.evaluate(() => testGate.assignments[1]);
    const b = await openWorker(second, true);
    await join(b);
    await expect(b.locator("#active")).toHaveText("1");
    const retained = await b.evaluate(() => testGate.assignments[0]);
    expect(retained.task_id).not.toBe(abandoned.task_id);
    await first.close();
    await expect.poll(async () => (await (await api.get(job)).json()).retries).toBe(1);

    await b.evaluate(() => {
      testGate.hold = false;
      for (const thread of testGate.pending) thread.release?.();
    });
    await expect.poll(async () => (await (await api.get(job)).json()).state).toBe("SUCCEEDED");
    const assignments = await b.evaluate(() => testGate.assignments);
    expect(assignments.find(a => a.task_id === abandoned.task_id)).toMatchObject({attempt: 2});
    const actual = Buffer.alloc(2 * example.width * example.height);
    const {tiles} = await (await api.get(job + "/tiles")).json();
    expect(tiles).toHaveLength(20);
    for (const tile of tiles) {
      const bytes = Buffer.from(tile.data_b64, "base64");
      for (let row = 0; row < tile.h; row++) {
        bytes.copy(actual, 2 * ((tile.y + row) * example.width + tile.x), 2 * row * tile.w, 2 * (row + 1) * tile.w);
      }
    }
    const expected = reference();
    expect(actual.equals(expected.iter)).toBe(true);
    const output = testInfo.outputPath("mandelbrot.png");
    await cli("result", "--job", id, "--out", output);
    const png = await readFile(output);
    const pixels = await b.evaluate(async bytes => {
      const img = await createImageBitmap(new Blob([new Uint8Array(bytes)], {type: "image/png"}));
      const canvas = new OffscreenCanvas(img.width, img.height);
      const ctx = canvas.getContext("2d");
      ctx.drawImage(img, 0, 0);
      return Array.from(ctx.getImageData(0, 0, img.width, img.height).data);
    }, [...png]);
    expect(Buffer.from(pixels).equals(expected.rgba)).toBe(true);
    await b.getByRole("button", {name: "Stop", exact: true}).click();

    const dashboard = await second.newPage();
    await dashboard.goto(origin);
    await dashboard.getByLabel("Operator credential").fill(operator);
    await dashboard.getByLabel("Job ID").fill(job.split("/").at(-1));
    await dashboard.getByRole("button", {name: "Watch"}).click();
    await expect(dashboard.locator("#accepted")).toHaveText("20/20");
    await expect.poll(async () => dashboard.locator("canvas").evaluate(c =>
      c.getContext("2d").getImageData(0, 0, c.width, c.height).data.at(-1)
    )).toBe(255);
    const canvas = await dashboard.locator("canvas").evaluate(c =>
      Array.from(c.getContext("2d").getImageData(0, 0, c.width, c.height).data)
    );
    expect(Buffer.from(canvas).equals(expected.rgba)).toBe(true);
    await testInfo.attach("coordinator", {
      body: await dashboard.screenshot({fullPage: true, path: process.env.COORDINATOR_SCREENSHOT}),
      contentType: "image/png",
    });
  } finally {
    await first.close();
    await second.close();
    await api.dispose();
  }
});

test("pause, visibility loss, explicit resume and Stop terminate held Dedicated Workers", async ({browser, playwright}) => {
  const api = await operatorAPI(playwright);
  const context = await browser.newContext();
  try {
    const page = await openWorker(context, true);
    const job = await submit(api);
    await join(page);
    await expect(page.locator("#active")).toHaveText("1");
    await page.getByRole("button", {name: "Pause", exact: true}).click();
    await expect(page.locator("#status")).toHaveText("Paused");
    expect(await page.evaluate(() => testGate.terminated)).toBe(1);

    await page.getByRole("button", {name: "Resume"}).click();
    await expect(page.locator("#active")).toHaveText("1");
    await page.evaluate(() => {
      Object.defineProperty(document, "hidden", {configurable: true, value: true});
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await expect(page.locator("#status")).toHaveText("Paused");
    await expect(page.getByRole("button", {name: "Resume"})).toBeDisabled();
    expect(await page.evaluate(() => testGate.terminated)).toBe(2);
    await page.evaluate(() => {
      delete document.hidden;
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await expect(page.locator("#active")).toHaveText("0");
    await page.getByRole("button", {name: "Resume"}).click();
    await expect(page.locator("#active")).toHaveText("1");
    await page.getByRole("button", {name: "Stop", exact: true}).click();
    await expect(page.locator("#status")).toHaveText("Stopped");
    expect(await page.evaluate(() => [testGate.started, testGate.terminated, testGate.sockets])).toEqual([3, 3, 1]);
    await expect.poll(async () => (await (await api.get(job)).json()).retries).toBe(3);
    expect((await cli("cancel", "--job", job.split("/").at(-1))).state).toBe("CANCELLED");
  } finally {
    await context.close();
    await api.dispose();
  }
});
