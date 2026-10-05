const dropZone = document.querySelector("#dropZone");
const fileInput = document.querySelector("#fileInput");
const folderInput = document.querySelector("#folderInput");
const fileList = document.querySelector("#fileList");
const emptyState = document.querySelector("#emptyState");
const queue = document.querySelector("#queue");
const concurrentUploads = document.querySelector("#concurrentUploads");
const jobs = new Map();
const chunkSize = 32 * 1024 * 1024;

concurrentUploads.value = savedConcurrency();
concurrentUploads.addEventListener("change", () => {
  concurrentUploads.value = normalizedConcurrency(concurrentUploads.value);
  try {
    localStorage.setItem("dumpster.concurrentUploads", concurrentUploads.value);
  } catch {
    // Uploads still work when browser storage is unavailable.
  }
});

document.querySelector("#chooseFiles").addEventListener("click", (event) => {
  event.stopPropagation();
  fileInput.click();
});
document.querySelector("#chooseFolder").addEventListener("click", (event) => {
  event.stopPropagation();
  folderInput.click();
});
fileInput.addEventListener("change", () => uploadFiles(fileInput.files));
folderInput.addEventListener("change", () => uploadFiles(folderInput.files));

for (const eventName of ["dragenter", "dragover"]) {
  dropZone.addEventListener(eventName, (event) => {
    event.preventDefault();
    dropZone.classList.add("dragging");
  });
}
for (const eventName of ["dragleave", "drop"]) {
  dropZone.addEventListener(eventName, (event) => {
    event.preventDefault();
    dropZone.classList.remove("dragging");
  });
}
dropZone.addEventListener("drop", (event) => uploadFiles(event.dataTransfer.files));
document.querySelector("#refreshButton").addEventListener("click", loadFiles);

async function api(url, options = {}) {
  const response = await fetch(url, options);
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `Request failed (${response.status})`);
  return body;
}

async function uploadFiles(files) {
  if (!files.length) return;
  const pending = Array.from(files, (file) => ({
    file,
    path: file.webkitRelativePath || file.name,
    size: file.size,
    lastModified: file.lastModified,
    offset: 0,
    paused: false,
  }));
  for (const job of pending) {
    job.element = uploadRow(job);
    bindJobActions(job);
    queue.append(job.element.row);
  }
  dropZone.setAttribute("aria-busy", "true");

  let next = 0;
  async function worker() {
    while (next < pending.length) {
      const job = pending[next++];
      try {
        await beginUpload(job);
      } catch (error) {
        pauseJob(job, error.message);
      }
    }
  }
  const workerCount = Math.min(normalizedConcurrency(concurrentUploads.value), pending.length);
  await Promise.all(Array.from({ length: workerCount }, worker));
  fileInput.value = "";
  folderInput.value = "";
  dropZone.removeAttribute("aria-busy");
  await loadFiles();
}

async function beginUpload(job) {
  setJobState(job, "Preparing…", "uploading");
  const result = await api("/api/uploads", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      path: job.path,
      size: job.size,
      lastModified: job.lastModified,
    }),
  });
  if (result.completed) {
    completeJob(job);
    return;
  }
  Object.assign(job, result.upload);
  jobs.set(job.id, job);
  bindJobActions(job);
  await continueUpload(job);
}

async function continueUpload(job) {
  if (!job.file) {
    chooseResumeFile(job);
    return;
  }
  job.paused = false;
  setJobState(job, `Uploading · ${progress(job)}%`, "uploading");
  while (job.offset < job.size && !job.paused) {
    const chunk = job.file.slice(job.offset, Math.min(job.offset + chunkSize, job.size));
    const result = await sendChunk(job, chunk);
    if (result.completed) {
      completeJob(job);
      await loadFiles();
      return;
    }
    Object.assign(job, result.upload);
  }
  if (job.paused) pauseJob(job, "Paused");
}

function sendChunk(job, chunk) {
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    job.request = request;
    request.open("PATCH", `/api/uploads/${job.id}`);
    request.setRequestHeader("Content-Type", "application/octet-stream");
    request.setRequestHeader("Upload-Offset", String(job.offset));
    request.upload.addEventListener("progress", (event) => {
      if (!event.lengthComputable) return;
      const percent = Math.min(99, Math.round(((job.offset + event.loaded) / job.size) * 100));
      job.element.bar.style.width = `${percent}%`;
      job.element.status.textContent = `Uploading · ${percent}%`;
    });
    request.addEventListener("load", () => {
      job.request = null;
      const body = parseJSON(request.responseText);
      if (request.status >= 200 && request.status < 300) {
        resolve(body);
        return;
      }
      if (request.status === 409 && Number.isFinite(body.offset)) {
        job.offset = body.offset;
        resolve({ completed: false, upload: job });
        return;
      }
      reject(new Error(body.error || `Upload failed (${request.status})`));
    });
    request.addEventListener("error", () => {
      job.request = null;
      reject(new Error("Network error"));
    });
    request.addEventListener("abort", () => {
      job.request = null;
      reject(new Error(job.paused ? "Paused" : "Upload interrupted"));
    });
    request.send(chunk);
  });
}

function uploadRow(job) {
  const row = document.createElement("div");
  row.className = "upload-row";

  const heading = document.createElement("div");
  heading.className = "upload-heading";
  const label = document.createElement("span");
  label.className = "upload-name";
  label.textContent = job.path;
  label.title = job.path;
  const status = document.createElement("span");
  status.className = "upload-status";
  status.textContent = job.offset ? `Paused · ${progress(job)}%` : `Waiting · ${formatBytes(job.size)}`;
  heading.append(label, status);

  const track = document.createElement("div");
  track.className = "progress-track";
  const bar = document.createElement("div");
  bar.className = "progress-bar";
  bar.style.width = `${progress(job)}%`;
  track.append(bar);

  const actions = document.createElement("div");
  actions.className = "upload-actions";
  const pause = actionButton("Pause");
  const resume = actionButton("Resume");
  const cancel = actionButton("Cancel", "danger");
  resume.hidden = true;
  cancel.hidden = true;
  actions.append(pause, resume, cancel);

  row.append(heading, track, actions);
  return { row, status, bar, pause, resume, cancel };
}

function bindJobActions(job) {
  job.element.pause.onclick = () => {
    job.paused = true;
    if (job.request) job.request.abort();
    else pauseJob(job, "Paused");
  };
  job.element.resume.onclick = () => {
    if (!job.id) beginUpload(job).catch((error) => pauseJob(job, error.message));
    else if (job.file) continueUpload(job).catch((error) => pauseJob(job, error.message));
    else chooseResumeFile(job);
  };
  job.element.cancel.onclick = () => cancelJob(job);
}

function pauseJob(job, message) {
  job.paused = true;
  setJobState(job, `${message} · ${progress(job)}%`, "paused");
}

function completeJob(job) {
  job.paused = false;
  job.offset = job.size;
  setJobState(job, "Complete", "complete");
  if (job.id) jobs.delete(job.id);
}

function setJobState(job, message, state) {
  job.element.row.classList.remove("error", "complete", "paused");
  if (state === "paused") job.element.row.classList.add("paused");
  if (state === "complete") job.element.row.classList.add("complete");
  job.element.status.textContent = message;
  job.element.bar.style.width = `${progress(job)}%`;
  job.element.pause.hidden = state !== "uploading";
  job.element.resume.hidden = state !== "paused";
  job.element.cancel.hidden = state !== "paused";
}

async function cancelJob(job) {
  if (!job.id) {
    job.element.row.remove();
    return;
  }
  if (!confirm(`Cancel the upload of ${job.path}? Its partial data will be deleted.`)) return;
  job.paused = true;
  if (job.request) job.request.abort();
  try {
    const response = await fetch(`/api/uploads/${job.id}`, { method: "DELETE" });
    if (!response.ok && response.status !== 404) throw new Error(`Cancel failed (${response.status})`);
    jobs.delete(job.id);
    job.element.row.remove();
  } catch (error) {
    pauseJob(job, error.message);
  }
}

function chooseResumeFile(job) {
  const picker = document.createElement("input");
  picker.type = "file";
  picker.addEventListener("change", () => {
    const file = picker.files[0];
    if (!file) return;
    const expectedName = job.path.split("/").pop();
    if (file.name !== expectedName || file.size !== job.size ||
        (job.lastModified && file.lastModified !== job.lastModified)) {
      pauseJob(job, "That is not the original file");
      return;
    }
    job.file = file;
    continueUpload(job).catch((error) => pauseJob(job, error.message));
  });
  picker.click();
}

async function loadUploads() {
  try {
    const result = await api("/api/uploads");
    for (const upload of result.uploads) {
      if (jobs.has(upload.id)) continue;
      const job = { ...upload, file: null, paused: true };
      job.element = uploadRow(job);
      jobs.set(job.id, job);
      bindJobActions(job);
      setJobState(job, `Paused · ${progress(job)}%`, "paused");
      queue.append(job.element.row);
    }
  } catch (error) {
    showQueueError(error.message);
  }
}

function actionButton(label, className = "") {
  const button = document.createElement("button");
  button.type = "button";
  button.className = `upload-action ${className}`.trim();
  button.textContent = label;
  return button;
}

function progress(job) {
  if (job.size === 0) return 100;
  return Math.min(100, Math.round((job.offset / job.size) * 100));
}

function parseJSON(value) {
  try {
    return JSON.parse(value);
  } catch {
    return {};
  }
}

function showQueueError(message) {
  const notice = document.createElement("div");
  notice.className = "notice error";
  notice.textContent = message;
  queue.prepend(notice);
}

async function loadFiles() {
  try {
    const result = await api("/api/files");
    fileList.replaceChildren(...result.files.map(fileRow));
    emptyState.hidden = result.files.length !== 0;
  } catch (error) {
    showQueueError(error.message);
  }
}

function fileRow(file) {
  const row = document.createElement("div");
  row.className = "file-row";

  const name = document.createElement("a");
  name.className = "file-name";
  name.href = fileURL(file.name);
  name.textContent = file.name;
  name.title = file.name;

  const meta = document.createElement("span");
  meta.className = "file-meta date";
  meta.textContent = `${formatBytes(file.size)} · ${new Date(file.modifiedAt).toLocaleString()}`;

  const download = document.createElement("a");
  download.className = "download";
  download.href = name.href;
  download.textContent = "Download ↓";

  row.append(name, meta, download);
  return row;
}

function fileURL(name) {
  return `/files/${name.split("/").map(encodeURIComponent).join("/")}`;
}

function normalizedConcurrency(value) {
  const parsed = Number.parseInt(value, 10);
  return String(Number.isFinite(parsed) ? Math.min(32, Math.max(1, parsed)) : 4);
}

function savedConcurrency() {
  try {
    return normalizedConcurrency(localStorage.getItem("dumpster.concurrentUploads") || "4");
  } catch {
    return "4";
  }
}

function formatBytes(bytes) {
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / (1024 ** index)).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

async function loadIdentity() {
  try {
    const user = await api("/api/me");
    document.querySelector("#identity").textContent = user.displayName || user.login;
  } catch {
    document.querySelector("#identity").textContent = "Unknown user";
  }
}

async function loadTailscaleStatus() {
  const badge = document.querySelector("#tailscaleStatus");
  try {
    const status = await api("/api/tailscale/status");
    badge.classList.toggle("connected", status.connected);
    badge.classList.toggle("disconnected", !status.connected);
    badge.lastChild.textContent = ` Tailscale Status: ${status.connected ? "Connected" : "Disconnected"}`;
  } catch {
    badge.classList.remove("connected");
    badge.classList.add("disconnected");
    badge.lastChild.textContent = " Tailscale Status: Disconnected";
  }
}

loadIdentity();
loadFiles();
loadUploads();
loadTailscaleStatus();
setInterval(loadTailscaleStatus, 15000);
