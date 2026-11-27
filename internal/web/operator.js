const el = id => document.getElementById(id);
const canvas = el("image");
const context = canvas.getContext("2d");
let watch;

async function api(current, path, method = "GET") {
  const response = await fetch(path, {
    method, headers: {Authorization: "Bearer " + current.token},
    signal: AbortSignal.any([current.controller.signal, AbortSignal.timeout(10000)])
  });
  if (!response.ok) {
    const error = await response.json();
    throw new Error(error.error);
  }
  return response;
}

function paint(tile, maxIter) {
  const image = context.createImageData(tile.w, tile.h);
  const data = atob(tile.data_b64);
  for (let i = 0; i < data.length / 2; i++) {
    const n = data.charCodeAt(i * 2) + 256 * data.charCodeAt(i * 2 + 1);
    if (n !== maxIter) {
      image.data[4 * i] = 9 * n % 256;
      image.data[4 * i + 1] = 5 * n % 256;
      image.data[4 * i + 2] = 13 * n % 256;
    }
    image.data[4 * i + 3] = 255;
  }
  context.putImageData(image, tile.x, tile.y);
}

async function poll(current) {
  try {
    const workers = await (await api(current,"/api/v1/workers")).json();
    if (current !== watch) return;
    const counts = {active: 0, idle: 0, paused: 0, lost: 0};
    el("workers").replaceChildren(...workers.map(worker => {
      counts[worker.state]++;
      const li = document.createElement("li");
      li.textContent = worker.session_id + " · " + worker.state + " · " + worker.active + "/" + worker.slots;
      return li;
    }));
    el("counts").textContent = Object.entries(counts).map(([name, count]) => count + " " + name).join(" · ");
    if (!current.path) { el("status").textContent = "Watching worker sessions"; return; }
    const job = await (await api(current,current.path)).json();
    if (current !== watch) return;
    if (!current.initialized) {
      canvas.width = job.input.width;
      canvas.height = job.input.height;
      canvas.hidden = false;
      current.initialized = true;
    }
    el("status").textContent = job.reason ? job.state + " · " + job.reason : job.state;
    el("accepted").textContent = job.accepted + "/" + job.total;
    el("retries").textContent = job.retries;
    el("elapsed").textContent = (job.elapsed_ms / 1000).toFixed(1) + " s";
    el("cancel").disabled = !["QUEUED","RUNNING"].includes(job.state);
    el("download").disabled = job.state !== "SUCCEEDED";
    // Limit requests per poll so a large backlog cannot monopolize the UI.
    for (let batch = 0; batch < 8 && current.cursor < job.accepted; batch++) {
      const page = await (await api(current,current.path + "/tiles?cursor=" + current.cursor)).json();
      if (current !== watch) return;
      for (const tile of page.tiles) paint(tile,job.input.max_iter);
      current.cursor = page.next_cursor;
    }
  } catch (error) {
    if (current === watch) el("status").textContent = error.message;
  } finally {
    if (current === watch) current.timer = setTimeout(() => poll(current),1000);
  }
}

el("watch").onclick = () => {
  const token = el("token").value || watch?.token;
  if (!token) { el("status").textContent = "Enter an operator credential."; return; }
  if (watch) { clearTimeout(watch.timer); watch.controller.abort(); }
  el("token").value = "";
  const id = el("job").value.trim();
  watch = {token, path: id ? "/api/v1/jobs/" + encodeURIComponent(id) : "", cursor: 0, controller: new AbortController()};
  canvas.hidden = true;
  for (const id of ["accepted", "elapsed", "retries"]) el(id).textContent = "—";
  el("cancel").disabled = el("download").disabled = true;
  poll(watch);
};
el("cancel").onclick = async () => {
  const current = watch;
  try { await api(current,current.path + "/cancel","POST"); }
  catch (error) { if (current === watch) el("status").textContent = error.message; }
};
el("download").onclick = async () => {
  const current = watch;
  try {
    const blob = await (await api(current,current.path + "/result")).blob();
    if (current !== watch) return;
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "mandelbrot.png";
    a.click();
    setTimeout(() => URL.revokeObjectURL(url),1000);
  } catch (error) { if (current === watch) el("status").textContent = error.message; }
};
