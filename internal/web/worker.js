const el = id => document.getElementById(id);
const kernel = "mandelbrot.v1";
const slots = el("slots");
for (let i = 1; i <= Math.min(4, navigator.hardwareConcurrency || 1); i++) {
  slots.add(new Option(String(i), String(i)));
}

let session;
const running = new Map();
let state = "Disconnected";

function updateControls() {
  el("status").textContent = state;
  el("active").textContent = running.size;
  el("join").disabled = !!session || document.hidden;
  el("pause").disabled = state !== "Joined";
  el("resume").disabled = state !== "Paused" || document.hidden;
  el("stop").disabled = !session;
  for (const id of ["token", "slots", "minutes"]) el(id).disabled = !!session;
  el("remaining").textContent = session ?
    Math.max(0, Math.ceil((session.ends - performance.now()) / 1000)) + " s" : "—";
}

function send(type, payload = {}) {
  const socket = session?.socket;
  if (socket?.readyState !== WebSocket.OPEN) return;
  if (socket.bufferedAmount > 65536) {
    stop("Connection stalled. Join again to continue.");
    return;
  }
  socket.send(JSON.stringify({v: 1, type, payload}));
}

function remove(lease) {
  const task = running.get(lease);
  if (!task) return;
  clearTimeout(task.timeout);
  task.thread.terminate();
  running.delete(lease);
  updateControls();
}

function terminateAll() {
  for (const lease of running.keys()) remove(lease);
}

function stop(reason) {
  const previous = session;
  session = undefined;
  terminateAll();
  state = reason;
  if (previous) {
    clearInterval(previous.heartbeat);
    clearInterval(previous.countdown);
    clearTimeout(previous.joinTimeout);
    previous.socket.close();
  }
  updateControls();
}

function pause() {
  if (state !== "Joined") return;
  state = "Paused";
  terminateAll();
  send("pause");
  updateControls();
}

function ready() {
  if (state === "Joined" && !document.hidden) {
    send("ready", {available_slots: session.limit - running.size});
  }
}

function assign(p) {
  if (state !== "Joined" || document.hidden) return;
  if (performance.now() >= session.ends) {
    stop("Session limit reached.");
    return;
  }
  const {job_id, task_id, lease_id} = p;
  const lease = {job_id, task_id, lease_id};
  if (p.kernel !== kernel || p.kernel_version !== 1 || running.size >= session.limit || running.has(lease_id)) {
    stop("Unexpected assignment.");
    return;
  }
  let thread;
  try {
    thread = new Worker("/kernel.js");
  } catch {
    send("task_error", {...lease, code: "worker", message: "Worker unavailable"});
    ready();
    return;
  }
  function finish(type, fields) {
    if (running.get(lease_id)?.thread !== thread) return;
    if (document.hidden) { pause(); return; }
    if (performance.now() >= session.ends) { stop("Session limit reached."); return; }
    remove(lease_id);
    send(type, {...lease, ...fields});
    ready();
  }
  const timeout = setTimeout(() => finish("task_error", {code: "deadline", message: "Task deadline reached"}), p.deadline_ms);
  running.set(lease_id, {thread, timeout});
  thread.onmessage = ({data}) => {
    const data_b64 = btoa(String.fromCharCode(...new Uint8Array(data)));
    finish("result", {data_b64});
  };
  thread.onerror = event => {
    event.preventDefault();
    finish("task_error", {code: "kernel", message: "Kernel execution failed"});
  };
  thread.postMessage(p.params);
  updateControls();
}

el("join").onclick = () => {
  if (session || document.hidden) return;
  let token = el("token").value;
  const minutes = Number(el("minutes").value);
  if (!token || !Number.isInteger(minutes) || minutes < 1 || minutes > 60) {
    state = "Enter a credential and a session limit from 1 to 60 minutes.";
    updateControls();
    return;
  }
  el("token").value = "";
  const url = new URL("/ws/v1/worker", location.href);
  url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  const socket = new WebSocket(url);
  const current = {socket, limit: Number(slots.value), ends: performance.now() + minutes * 60000};
  session = current;
  state = "Connecting";
  current.countdown = setInterval(() => {
    if (performance.now() >= current.ends) stop("Session limit reached.");
    else updateControls();
  }, 250);
  current.joinTimeout = setTimeout(() => stop("Join timed out."), 6000);
  socket.onopen = () => {
    if (session !== current) return;
    send("hello", {worker_token: token, slots: current.limit, supported_kernels: [kernel]});
    token = "";
  };
  socket.onclose = () => {
    token = "";
    if (session === current) stop("Disconnected. Join again to continue.");
  };
  socket.onmessage = ({data}) => {
    if (session !== current) return;
    const message = JSON.parse(data);
    const p = message.payload;
    switch (message.type) {
      case "welcome":
        clearTimeout(current.joinTimeout);
        current.heartbeat = setInterval(() => send("heartbeat"), p.heartbeat_ms);
        state = "Joined";
        if (document.hidden) pause();
        else ready();
        break;
      case "assign": assign(p); break;
      case "cancel":
        remove(p.lease_id);
        ready();
        break;
      case "error": stop(p.message); break;
    }
    updateControls();
  };
  updateControls();
};

el("pause").onclick = pause;
el("resume").onclick = () => {
  if (state !== "Paused" || document.hidden) return;
  state = "Joined";
  send("resume");
  ready();
  updateControls();
};
el("stop").onclick = () => stop("Stopped");
document.addEventListener("visibilitychange", () => {
  if (document.hidden) pause();
  updateControls();
});
window.addEventListener("pagehide", () => stop("Stopped"));
updateControls();
