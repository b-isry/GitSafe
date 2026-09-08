(function () {
  "use strict";

  var app = document.getElementById("app");
  var sidebar = document.getElementById("sidebar");
  var navToggle = document.getElementById("navToggle");
  var scrim = document.getElementById("scrim");
  var progress = document.getElementById("routeProgress");

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }
  function humanBytes(b) {
    if (!b) return "0 B";
    var units = ["B", "KB", "MB", "GB", "TB"];
    var i = 0;
    var v = b;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (i === 0 ? v : v.toFixed(1)) + " " + units[i];
  }

  // --- responsive navigation ---
  function setNav(open) {
    if (!app) return;
    app.classList.toggle("nav-open", open);
    if (scrim) scrim.hidden = !open;
    if (navToggle) navToggle.setAttribute("aria-expanded", String(open));
  }
  if (navToggle) {
    navToggle.addEventListener("click", function () {
      setNav(!app.classList.contains("nav-open"));
    });
  }
  if (scrim) scrim.addEventListener("click", function () { setNav(false); });
  if (sidebar) {
    sidebar.addEventListener("click", function (e) {
      if (e.target && e.target.matches(".nav__item")) setNav(false);
    });
  }

  function showRouteProgress() {
    if (!progress) return;
    progress.hidden = false;
    window.clearTimeout(showRouteProgress._t);
    showRouteProgress._t = window.setTimeout(function () {
      progress.hidden = true;
    }, 800);
  }

  // --- scan status tracking ---
  function fetchScanStatus() {
    return fetch("/api/status", { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.json(); });
  }

  // Resolves with {scanning, lastScan, skipped} once a scan is no longer
  // running. Polls every second.
  function waitForScanEnd() {
    var attempts = 0;
    return new Promise(function (resolve, reject) {
      (function tick() {
        fetchScanStatus()
          .then(function (st) {
            if (!st.scanning) { resolve(st); return; }
            attempts += 1;
            if (attempts > 600) { resolve(st); return; }
            window.setTimeout(tick, 1000);
          })
          .catch(function () {
            attempts += 1;
            if (attempts > 60) { reject(new Error("status unavailable")); return; }
            window.setTimeout(tick, 1000);
          });
      })();
    });
  }

  // --- individual backup (repository detail page) ---
  var createBtn = document.getElementById("createBackupBtn");
  var statusSection = document.getElementById("backupStatusSection");
  var statusBox = document.getElementById("backupStatus");

  function renderJob(job) {
    if (!statusSection || !statusBox) return;
    statusSection.hidden = false;
    statusBox.innerHTML = "";

    var res = job.Bundle || {};
    var esc = function (s) { return String(s == null ? "" : s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;"); };

    var stateClass = { idle: "status--stale", creating: "status--progress", uploading: "status--progress", completed: "status--protected", failed: "status--failed" };
    var stateLabel = { idle: "Idle", creating: "Creating backup", uploading: "Uploading", completed: "Completed", failed: "Failed" };
    var chip = el("span", "status " + (stateClass[job.State] || "status--stale"), stateLabel[job.State] || job.State);
    var line = el("div", "kv__row");
    line.appendChild(el("span", "kv__key", "Status"));
    var valWrap = el("span", "kv__val");
    valWrap.appendChild(chip);
    line.appendChild(valWrap);
    statusBox.appendChild(line);

    function addRow(key, val, mono) {
      var row = el("div", "kv__row");
      row.appendChild(el("span", "kv__key", key));
      row.appendChild(el("span", "kv__val" + (mono ? " mono" : ""), val));
      statusBox.appendChild(row);
    }

    if (job.State === "creating" || job.State === "idle") {
      var bar = el("div", "progress progress--indeterminate");
      bar.appendChild(el("div", "progress__bar"));
      statusBox.appendChild(bar);
    }

    if (res.bundlePath) {
      addRow("Bundle", res.bundlePath, true);
    }
    if (res.bundleName) {
      addRow("Bundle file", res.bundleName, true);
    }
    if (res.bundleSize) {
      addRow("Size", humanBytes(res.bundleSize));
    }
    if (res.outputPath) {
      addRow("Local location", res.outputPath, true);
    }
    if (res.cloud && res.cloud.enabled) {
      var driveLabel = {
        none: "Disabled",
        uploading: "Uploading to Drive",
        uploaded: "Uploaded",
        failed: "Upload failed"
      }[res.cloud.status] || res.cloud.status;
      addRow("Google Drive", driveLabel);

      if (res.cloud.status === "uploading" && res.cloud.bytesTotal > 0) {
        var pct = Math.round((res.cloud.bytesDone / res.cloud.bytesTotal) * 100);
        var dbar = el("div", "progress");
        var fill = el("div", "progress__bar");
        fill.style.width = pct + "%";
        dbar.appendChild(fill);
        statusBox.appendChild(dbar);
        addRow("Uploaded", humanBytes(res.cloud.bytesDone) + " / " + humanBytes(res.cloud.bytesTotal));
      }
      if (res.cloud.status === "failed" && res.cloud.error) {
        addRow("Upload error", res.cloud.error, true);
      }
      if (res.cloud.driveFileId) {
        addRow("Drive file ID", res.cloud.driveFileId, true);
      }
    }

    if (job.State === "failed" && res.error) {
      addRow("Error", res.error, true);
    }
  }

  function poll(id) {
    var attempts = 0;
    var timer = window.setInterval(function () {
      fetch("/backups/" + id, { headers: { "Accept": "application/json" } })
        .then(function (resp) { return resp.json(); })
        .then(function (job) {
          renderJob(job);
          attempts++;
          if (job.State === "completed" || job.State === "failed") {
            window.clearInterval(timer);
            if (createBtn) {
              createBtn.disabled = false;
              createBtn.textContent = "Create backup";
            }
            window.setTimeout(function () { window.location.reload(); }, 1200);
          }
        })
        .catch(function () { attempts++; if (attempts > 60) window.clearInterval(timer); });
    }, 700);
  }

  if (createBtn) {
    createBtn.addEventListener("click", function () {
      var path = createBtn.getAttribute("data-path");
      if (!path || createBtn.disabled) return;
      createBtn.disabled = true;
      createBtn.textContent = "Starting…";
      showRouteProgress();

      fetch("/backups", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ repoPath: path })
      })
        .then(function (resp) {
          return resp.json().then(function (data) {
            return { status: resp.status, data: data };
          });
        })
        .then(function (r) {
          if (r.data && r.data.id) {
            createBtn.textContent = "Backing up…";
            poll(r.data.id);
          } else {
            createBtn.disabled = false;
            createBtn.textContent = "Create backup";
            if (r.status === 409) {
              var err = document.getElementById("backupFlash");
              if (err) { err.textContent = r.data.error || "Already being backed up."; err.hidden = false; }
            }
          }
        })
        .catch(function () {
          createBtn.disabled = false;
          createBtn.textContent = "Create backup";
        });
    });
  }

  // --- dashboard: selection + batch backup ---
  var repoTable = document.getElementById("repoTable");
  var backupSelectedBtn = document.getElementById("backupSelectedBtn");
  var selectStaleBtn = document.getElementById("selectStaleBtn");
  var clearSelectionBtn = document.getElementById("clearSelectionBtn");
  var selectedCount = document.getElementById("selectedCount");

  var selected = new Set();
  var busyPaths = new Set();
  var pendingJobs = new Set();

  function statusCellFor(path) {
    if (!repoTable) return null;
    return repoTable.querySelector('[data-repo-status][data-path="' + cssEscape(path) + '"]');
  }
  function rowFor(path) {
    if (!repoTable) return null;
    return repoTable.querySelector('tr[data-path="' + cssEscape(path) + '"]');
  }
  function checkboxFor(path) {
    if (!repoTable) return null;
    return repoTable.querySelector('input.repo-select[data-path="' + cssEscape(path) + '"]');
  }
  function cssEscape(s) {
    return String(s).replace(/\\/g, "\\\\").replace(/"/g, '\\"');
  }

  function refreshSelection() {
    var n = selected.size;
    if (selectedCount) {
      selectedCount.textContent = n === 0 ? "Nothing selected"
        : n === 1 ? "1 repository selected"
        : n + " repositories selected";
    }
    if (backupSelectedBtn) backupSelectedBtn.disabled = n === 0 || busyPaths.size > 0;
    if (clearSelectionBtn) clearSelectionBtn.hidden = n === 0;
    if (repoTable) {
      var boxes = repoTable.querySelectorAll("input.repo-select");
      Array.prototype.forEach.call(boxes, function (box) {
        box.checked = selected.has(box.getAttribute("data-path"));
      });
    }
  }

  function selectRow(path, on) {
    if (busyPaths.has(path)) return;
    if (on) { selected.add(path); } else { selected.delete(path); }
    var row = rowFor(path);
    if (row) row.classList.toggle("is-selected", !!on);
    refreshSelection();
  }

  if (repoTable) {
    repoTable.addEventListener("change", function (e) {
      var box = e.target;
      if (box && box.classList && box.classList.contains("repo-select")) {
        selectRow(box.getAttribute("data-path"), box.checked);
      }
    });
    repoTable.addEventListener("click", function (e) {
      if (e.target && e.target.closest && e.target.closest("input.repo-select")) return;
      var link = e.target && e.target.closest ? e.target.closest("[data-no-select]") : null;
      if (link) return;
      var row = e.target && e.target.closest ? e.target.closest("tr[data-path]") : null;
      if (!row) return;
      selectRow(row.getAttribute("data-path"), !selected.has(row.getAttribute("data-path")));
    });
  }

  if (selectStaleBtn) {
    selectStaleBtn.addEventListener("click", function () {
      if (repoTable) {
        var rows = repoTable.querySelectorAll("tr.is-stale");
        Array.prototype.forEach.call(rows, function (row) {
          selectRow(row.getAttribute("data-path"), true);
        });
      }
    });
  }

  if (clearSelectionBtn) {
    clearSelectionBtn.addEventListener("click", function () {
      Array.prototype.forEach.call(Array.from(selected), function (p) { selectRow(p, false); });
    });
  }

  function setStatusChip(path, label, kind) {
    var cell = statusCellFor(path);
    if (!cell) return;
    cell.className = "status status--" + kind;
    cell.textContent = label;
    cell.classList.add("status");
  }

  function startPollingJob(job) {
    pendingJobs.add(job.id);
    (function tick() {
      fetch("/backups/" + job.id, { headers: { "Accept": "application/json" } })
        .then(function (resp) { return resp.json(); })
        .then(function (j) {
          var terminal = j.State === "completed" || j.State === "failed";
          if (j.State === "completed") {
            setStatusChip(job.repoPath, "Backed up", "protected");
          } else if (j.State === "failed") {
            setStatusChip(job.repoPath, "Failed", "failed");
          } else {
            setStatusChip(job.repoPath, "Backing up…", "progress");
          }
          if (terminal) {
            pendingJobs.delete(job.id);
            if (pendingJobs.size === 0) {
              window.setTimeout(function () { window.location.reload(); }, 1400);
            }
          } else {
            window.setTimeout(tick, 700);
          }
        })
        .catch(function () { window.setTimeout(tick, 1500); });
    })();
  }

  function markBusy(path, label) {
    busyPaths.add(path);
    selected.delete(path);
    var box = checkboxFor(path);
    if (box) box.disabled = true;
    var row = rowFor(path);
    if (row) { row.classList.add("is-busy"); row.classList.remove("is-selected"); }
    setStatusChip(path, label, "progress");
    refreshSelection();
  }

  if (backupSelectedBtn && repoTable) {
    backupSelectedBtn.addEventListener("click", function () {
      if (backupSelectedBtn.disabled) return;
      var paths = Array.from(selected);
      if (paths.length === 0) return;
      backupSelectedBtn.disabled = true;
      showRouteProgress();

      fetch("/backups", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ repoPaths: paths })
      })
        .then(function (resp) { return resp.json(); })
        .then(function (data) {
          var jobs = data.jobs || [];
          var already = data.alreadyRunning || [];
          already.forEach(function (p) { markBusy(p, "Backing up…"); });
          jobs.forEach(function (job) {
            markBusy(job.repoPath, "Backing up…");
            if (job.id) startPollingJob(job);
          });
          if (jobs.length === 0 && already.length === 0) {
            window.setTimeout(function () { window.location.reload(); }, 800);
          } else if (jobs.length === 0 && already.length > 0) {
            // Only already-running repos: wait for the in-flight runs to settle.
            window.setTimeout(function () { window.location.reload(); }, 4000);
          }
        })
        .catch(function () {
          refreshSelection();
        });
    });
  }

  // --- dashboard: scanning state + Scan now ---
  var scanBanner = document.getElementById("scanBanner");
  var scanBtn = document.getElementById("scanBtn");

  function reloadWhenScanDone() {
    waitForScanEnd().then(function () { window.location.reload(); });
  }

  if (scanBanner && scanBanner.getAttribute("data-scanning") === "true") {
    showRouteProgress();
    reloadWhenScanDone();
  }

  if (scanBtn) {
    scanBtn.addEventListener("click", function () {
      if (scanBtn.disabled) return;
      scanBtn.disabled = true;
      showRouteProgress();
      fetch("/api/scan", { method: "POST" })
        .then(function (resp) { return resp.json(); })
        .then(function () { reloadWhenScanDone(); })
        .catch(function () {
          scanBtn.disabled = false;
        });
    });
  }

  // --- folder picker (settings) ---
  var changeFolderBtn = document.getElementById("changeFolderBtn");
  var rootPathInput = document.getElementById("rootPath");
  var rootNameDisplay = document.getElementById("rootNameDisplay");
  var rootPathDisplay = document.getElementById("rootPathDisplay");

  function setFolderDisplay(path) {
    if (!rootPathInput) return;
    rootPathInput.value = path || "";
    if (rootPathDisplay) rootPathDisplay.textContent = path || "";
    if (rootNameDisplay) {
      if (!path) { rootNameDisplay.textContent = ""; return; }
      var parts = path.replace(/[/\\]+$/, "").split(/[/\\]/);
      rootNameDisplay.textContent = parts[parts.length - 1] || path;
    }
  }

  if (changeFolderBtn && window.GitSafeFolderBrowser) {
    changeFolderBtn.addEventListener("click", function () {
      window.GitSafeFolderBrowser.open({
        title: "Where are your projects?",
        startPath: rootPathInput ? rootPathInput.value : "",
        onSelect: setFolderDisplay
      });
    });
  }

  // --- settings form ---
  var settingsForm = document.getElementById("settingsForm");
  var saveBtn = document.getElementById("saveSettingsBtn");
  var cancelBtn = document.getElementById("cancelSettingsBtn");
  var flash = document.getElementById("settingsFlash");
  var scanStatusBox = document.getElementById("scanStatusBox");
  var scanStatusText = document.getElementById("scanStatusText");

  function wireToggle(toggleId, checkboxId) {
    var toggle = document.getElementById(toggleId);
    var checkbox = document.getElementById(checkboxId);
    if (!toggle || !checkbox) return;
    function sync() {
      toggle.classList.toggle("is-on", checkbox.checked);
      toggle.setAttribute("aria-checked", String(checkbox.checked));
    }
    function flip() {
      checkbox.checked = !checkbox.checked;
      sync();
    }
    toggle.addEventListener("click", flip);
    toggle.addEventListener("keydown", function (e) {
      if (e.key === " " || e.key === "Enter") { e.preventDefault(); flip(); }
    });
    checkbox.addEventListener("change", sync);
    sync();
  }
  wireToggle("backupHistoryToggle", "backupHistory");
  wireToggle("cloudEnabledToggle", "cloudEnabled");

  function flashMessage(text, kind) {
    if (!flash) return;
    flash.textContent = text;
    flash.className = "form__flash form__flash--" + kind;
    flash.hidden = false;
  }

  function clearFieldErrors() {
    if (!settingsForm) return;
    var fields = settingsForm.querySelectorAll(".field--error");
    Array.prototype.forEach.call(fields, function (f) {
      f.classList.remove("field--error");
      var err = f.querySelector(".field__error");
      if (err) err.remove();
      var input = f.querySelector("input");
      if (input) input.removeAttribute("aria-invalid");
    });
  }

  function markFieldError(fieldName, msg) {
    if (!settingsForm) return;
    var field = settingsForm.querySelector('[data-field="' + fieldName + '"]');
    if (!field) return;
    field.classList.add("field--error");
    var input = field.querySelector("input");
    if (input) input.setAttribute("aria-invalid", "true");
    var errNode = document.createElement("p");
    errNode.className = "field__error";
    errNode.textContent = msg;
    field.appendChild(errNode);
  }

  function collectSettings() {
    var days = parseInt(document.getElementById("days").value, 10);
    if (isNaN(days)) days = 0;
    return {
      rootPath: document.getElementById("rootPath").value,
      days: days,
      outputPath: document.getElementById("outputPath").value,
      backupHistory: document.getElementById("backupHistory").checked,
      cloudEnabled: document.getElementById("cloudEnabled").checked,
      credentialsFile: document.getElementById("credentialsFile").value
    };
  }

  // Shows the scan status box and keeps it updated until the scan completes.
  function trackScanStatus(text) {
    if (!scanStatusBox) return;
    if (scanStatusText) scanStatusText.textContent = text || "Scanning repositories…";
    scanStatusBox.hidden = false;
    waitForScanEnd().then(function () {
      if (scanStatusText) scanStatusText.textContent = "Repository scan complete.";
      window.setTimeout(function () {
        if (scanStatusBox) scanStatusBox.hidden = true;
      }, 4000);
    });
  }

  // If a scan is running when the settings page loads (e.g. after a root
  // change), surface it.
  if (scanStatusBox && scanStatusBox.getAttribute("data-scanning") === "true") {
    trackScanStatus();
  }

  if (saveBtn && settingsForm) {
    saveBtn.addEventListener("click", function () {
      if (saveBtn.disabled) return;
      saveBtn.disabled = true;
      saveBtn.classList.add("is-loading");
      showRouteProgress();
      clearFieldErrors();

      fetch("/settings", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(collectSettings())
      })
        .then(function (resp) {
          return resp.json().then(function (data) {
            return { status: resp.status, data: data };
          });
        })
        .then(function (r) {
          if (r.data && r.data.errors) {
            Object.keys(r.data.errors).forEach(function (k) {
              markFieldError(k, r.data.errors[k]);
            });
          }
          if (r.data && r.data.ok) {
            flashMessage(r.data.message || "Settings saved.", "success");
            if (r.data.settings) {
              applyServerSettings(r.data.settings);
              setFolderDisplay(r.data.settings.rootPath);
            }
            if (r.data.scanning) {
              trackScanStatus("Settings saved. Scanning repositories…");
              flashMessage("Settings saved. Scanning repositories…", "success");
            }
          } else {
            flashMessage(r.data && r.data.message ? r.data.message : "Could not save settings.", "error");
          }
        })
        .catch(function () {
          flashMessage("Could not save settings. Check the connection and try again.", "error");
        })
        .then(function () {
          saveBtn.disabled = false;
          saveBtn.classList.remove("is-loading");
        });
    });
  }

  function applyServerSettings(s) {
    var idMap = {
      days: "days",
      outputPath: "outputPath",
      credentialsFile: "credentialsFile"
    };
    ["days", "outputPath", "credentialsFile"].forEach(function (k) {
      var el = document.getElementById(idMap[k]);
      if (el) el.value = s[k] != null ? s[k] : "";
    });
    if (document.getElementById("backupHistory")) {
      document.getElementById("backupHistory").checked = !!s.backupHistory;
    }
    if (document.getElementById("cloudEnabled")) {
      document.getElementById("cloudEnabled").checked = !!s.cloudEnabled;
    }
    wireToggle("backupHistoryToggle", "backupHistory");
    wireToggle("cloudEnabledToggle", "cloudEnabled");
  }

  if (cancelBtn) {
    // Cancel discards any unsaved edits and reloads the current saved settings.
    cancelBtn.addEventListener("click", function () {
      if (settingsForm) settingsForm.reset();
      window.location.reload();
    });
  }
})();