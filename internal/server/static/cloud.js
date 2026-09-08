(function () {
  "use strict";

  var cloudRepos = document.getElementById("cloudRepos");
  if (!cloudRepos) return; // not the cloud repositories page

  var loading = document.getElementById("cloudLoading");
  var errorBox = document.getElementById("cloudError");
  var toolbar = document.getElementById("cloudToolbar");
  var table = document.getElementById("cloudTable");
  var tbody = table ? table.querySelector("tbody") : null;
  var empty = document.getElementById("cloudEmpty");
  var protectBtn = document.getElementById("protectSelectedBtn");
  var selectedCount = document.getElementById("cloudSelected");
  var flash = document.getElementById("cloudFlash");
  var protectedTable = document.getElementById("protectedTable");
  var protectedTbody = protectedTable ? protectedTable.querySelector("tbody") : null;
  var protectedEmpty = document.getElementById("protectedEmpty");
  var backupAllBtn = document.getElementById("backupAllBtn");
  var backupFlash = document.getElementById("backupFlash");
  var historyLoading = document.getElementById("historyLoading");
  var historyTable = document.getElementById("historyTable");
  var historyTbody = historyTable ? historyTable.querySelector("tbody") : null;
  var historyEmpty = document.getElementById("historyEmpty");

  var retKeepLocal = document.getElementById("retKeepLocal");
  var retKeepDays = document.getElementById("retKeepDays");
  var retKeepJobs = document.getElementById("retKeepJobs");
  var retKeepDriveDays = document.getElementById("retKeepDriveDays");
  var retDriveEnabled = document.getElementById("retDriveEnabled");
  var retDriveHint = document.getElementById("retDriveHint");
  var retSaveBtn = document.getElementById("retSaveBtn");
  var retRunBtn = document.getElementById("retRunBtn");
  var retFlash = document.getElementById("retFlash");
  var retResult = document.getElementById("retResult");
  var retSchedEnabled = document.getElementById("retSchedEnabled");
  var retSchedDays = document.getElementById("retSchedDays");
  var retSchedStatus = document.getElementById("retSchedStatus");

  var notifEnabled = document.getElementById("notifEnabled");
  var notifUrl = document.getElementById("notifUrl");
  var notifTimeout = document.getElementById("notifTimeout");
  var notifAuthHeader = document.getElementById("notifAuthHeader");
  var notifAuthToken = document.getElementById("notifAuthToken");
  var notifAuthHint = document.getElementById("notifAuthHint");
  var notifTestBtn = document.getElementById("notifTestBtn");
  var notifFlash = document.getElementById("notifFlash");
  var overrideList = document.getElementById("overrideList");
  var overrideAddBtn = document.getElementById("overrideAddBtn");

  var selected = new Set();
  var csrfToken = "";
  var protectedRepos = [];

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }

  function showError(msg) {
    if (!errorBox) return;
    errorBox.textContent = msg;
    errorBox.hidden = false;
  }

  function flashMessage(text, kind) {
    if (!flash) return;
    flash.textContent = text;
    flash.className = "form__flash form__flash--" + (kind === "error" ? "error" : "success");
    flash.hidden = false;
    window.clearTimeout(flashMessage._t);
    flashMessage._t = window.setTimeout(function () { flash.hidden = true; }, 4000);
  }

  function backupFlashMessage(text, kind) {
    if (!backupFlash) return;
    backupFlash.textContent = text;
    backupFlash.className = "form__flash form__flash--" + (kind === "error" ? "error" : "success");
    backupFlash.hidden = false;
    window.clearTimeout(backupFlashMessage._t);
    backupFlashMessage._t = window.setTimeout(function () { backupFlash.hidden = true; }, 5000);
  }

  function jobLabel(state) {
    var map = {
      enqueued: "Queued", authorizing: "Authorizing", cloning: "Cloning",
      bundling: "Bundling", validating: "Validating", recording: "Recording",
      uploading: "Uploading", completed: "Complete", failed: "Failed",
      interrupted: "Interrupted"
    };
    return map[state] || state;
  }

  function renderBackupStatusCell(rp, cell) {
    cell.innerHTML = "";
    // Latest job for this repo determines the live status chip.
    fetch("/api/protected-repositories/" + encodeURIComponent(rp.id) + "/backup-jobs", {
      headers: { "Accept": "application/json" }
    })
      .then(function (resp) { return resp.json(); })
      .then(function (data) {
        var jobs = data.jobs || [];
        var active = null;
        var latest = null;
        for (var i = 0; i < jobs.length; i++) {
          if (!active && jobs[i].state !== "completed" && jobs[i].state !== "failed" && jobs[i].state !== "interrupted") {
            active = jobs[i];
          }
          if (!latest || (jobs[i].startedAt || "") >= (latest.startedAt || "")) {
            latest = jobs[i];
          }
        }
        var s;
        if (active) {
          s = el("span", "status status--progress", jobLabel(active.state));
        } else if (latest && latest.state === "completed") {
          s = el("span", "status status--protected", "Backed up");
        } else if (latest && latest.state === "failed") {
          s = el("span", "status status--failed", "Backup failed");
        } else {
          s = el("span", "status status--stale", "Not backed up");
        }
        cell.appendChild(s);
      })
      .catch(function () {});
  }

  function renderProtectedTable(repos) {
    if (!protectedTbody) return;
    protectedRepos = repos;
    protectedTbody.innerHTML = "";
    if (protectedEmpty) protectedEmpty.hidden = repos.length > 0;
    repos.forEach(function (rp) {
      var tr = el("tr");
      tr.appendChild(tdCell(el("span", "repo-link", rp.fullName)));
      tr.appendChild(tdCell(el("span", "mono", rp.defaultBranch || "—")));
      tr.appendChild(tdCell(el("span", "mono", (rp.addedAt || "").slice(0, 10))));

      var backupBtn = el("button", "btn btn--primary", "Backup");
      backupBtn.type = "button";
      backupBtn.setAttribute("data-id", rp.id);
      backupBtn.addEventListener("click", function () {
        triggerBackup(rp.id, rp.fullName, backupBtn);
      });
      var tdBackup = el("td");
      tdBackup.appendChild(backupBtn);
      tr.appendChild(tdBackup);

      var statusCell = el("td");
      renderBackupStatusCell(rp, statusCell);
      tr.appendChild(statusCell);

      var removeBtn = el("button", "btn btn--ghost", "Remove");
      removeBtn.type = "button";
      removeBtn.addEventListener("click", function () {
        removeBtn.disabled = true;
        fetch("/api/protected-repositories/" + encodeURIComponent(rp.id), {
          method: "DELETE",
          headers: { "X-CSRF-Token": csrfToken }
        })
          .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
          .then(function (r) {
            if (r.status === 200) {
              tr.remove();
              flashMessage("Protection removed.", "success");
              loadAll();
            } else {
              removeBtn.disabled = false;
              flashMessage((r.data && r.data.error) || "Could not remove protection.", "error");
            }
          })
          .catch(function () { removeBtn.disabled = false; });
      });
      var tdAction = el("td");
      tdAction.appendChild(removeBtn);
      tr.appendChild(tdAction);
      protectedTbody.appendChild(tr);
    });
    if (backupAllBtn) backupAllBtn.disabled = repos.length === 0;
  }

  function loadProtected() {
    return fetch("/api/protected-repositories", { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.json(); })
      .then(function (data) { renderProtectedTable(data.repositories || []); });
  }

  // pollJob returns the resolved job once it reaches a terminal state, or the
  // job object if the poll itself errors out (to avoid an infinite loop).
  function pollJob(id) {
    return fetch("/api/backup-jobs/" + encodeURIComponent(id), { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.json(); })
      .then(function (job) {
        if (job.state === "completed" || job.state === "failed" || job.state === "interrupted") {
          return job;
        }
        return new Promise(function (resolve) {
          window.setTimeout(function () { resolve(pollJob(id)); }, 800);
        });
      });
  }

  function triggerBackup(id, fullName, btn) {
    if (btn) btn.disabled = true;
    fetch("/api/protected-repositories/" + encodeURIComponent(id) + "/backup", {
      method: "POST",
      headers: { "X-CSRF-Token": csrfToken }
    })
      .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
      .then(function (r) {
        if (r.status !== 202) {
          if (btn) btn.disabled = false;
          backupFlashMessage((r.data && r.data.error) || "Could not start the backup.", "error");
          return;
        }
        backupFlashMessage("Backup started for " + fullName + ".", "success");
        return pollJob(r.data.id).then(function (job) {
          if (job.state === "failed") {
            backupFlashMessage("Backup failed for " + fullName + (job.error ? ": " + job.error : ""), "error");
          } else {
            backupFlashMessage("Backup complete for " + fullName + ".", "success");
          }
          if (btn) { btn.disabled = false; }
          refreshAfterBackup();
        });
      })
      .catch(function () {
        if (btn) btn.disabled = false;
        backupFlashMessage("Could not reach the server.", "error");
      });
  }

  function refreshAfterBackup() {
    loadProtected();
    loadHistory();
  }

  function loadHistory() {
    if (!historyTbody) return;
    if (historyLoading) historyLoading.hidden = false;
    var repos = protectedRepos;
    if (repos.length === 0) {
      if (historyLoading) historyLoading.hidden = true;
      historyTbody.innerHTML = "";
      if (historyEmpty) historyEmpty.hidden = false;
      return;
    }
    var fetches = repos.map(function (rp) {
      return fetch("/api/protected-repositories/" + encodeURIComponent(rp.id) + "/backups", {
        headers: { "Accept": "application/json" }
      })
        .then(function (resp) { return resp.json(); })
        .then(function (data) { return data.backups || []; })
        .catch(function () { return []; });
    });
    Promise.all(fetches).then(function (groups) {
      if (historyLoading) historyLoading.hidden = true;
      var all = [];
      groups.forEach(function (g) { all = all.concat(g); });
      all.sort(function (a, b) { return (b.createdAt || "").localeCompare(a.createdAt || ""); });
      historyTbody.innerHTML = "";
      if (historyEmpty) historyEmpty.hidden = all.length > 0;
      all.forEach(function (b) {
        var tr = el("tr");
        tr.appendChild(tdCell(el("span", "repo-link", b.fullName)));
        tr.appendChild(tdCell(el("span", "mono", b.bundleName || "—")));
        tr.appendChild(tdCell(el("span", "mono", (b.createdAt || "").replace("T", " ").slice(0, 16))));
        tr.appendChild(tdCell(el("span", "mono", humanBytes(b.bundleSize))));
        var s = backupStatusChip(b.status);
        tr.appendChild(tdCell(s));
        historyTbody.appendChild(tr);
      });
    });
  }

  function humanBytes(n) {
    if (!n && n !== 0) return "—";
    var u = ["B", "KB", "MB", "GB"];
    var i = 0;
    var v = Number(n);
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return v.toFixed(i === 0 ? 0 : 1) + " " + u[i];
  }

  // backupStatusChip renders the terminal status of a backup record. "uploaded"
  // means the bundle was copied to Drive; "bundled" means stored locally only
  // (Drive disabled or an upload failed); "failed" means the backup failed.
  function backupStatusChip(status) {
    if (status === "uploaded") {
      return el("span", "status status--protected", "Uploaded to Drive");
    }
    if (status === "bundled") {
      return el("span", "status status--protected", "Stored");
    }
    if (status === "failed") {
      return el("span", "status status--failed", "Failed");
    }
    return el("span", "status status--stale", jobLabel(status));
  }

  function loadAll() {
    return loadRepositories()
      .then(loadProtected)
      .then(loadHistory)
      .then(loadDriveStatus);
  }

  // loadDriveStatus reflects the configured/connected Google Drive state from
  // /api/connections. "uploaded" backups require Drive to be configured.
  function loadDriveStatus() {
    var chip = document.getElementById("driveStatus");
    var detail = document.getElementById("driveDetail");
    if (!chip) return;
    fetch("/api/connections", { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.json(); })
      .then(function (data) {
        var d = (data && data.drive) || {};
        if (d.configured) {
          chip.className = "status status--protected";
          chip.textContent = "Configured";
          if (detail) {
            detail.hidden = false;
            detail.textContent = (d.credentialsFile || "");
          }
        } else {
          chip.className = "status status--stale";
          chip.textContent = "Not configured";
          if (detail) { detail.hidden = true; }
        }
      })
      .catch(function () {});
  }

  function loadRepositories() {
    return fetch("/api/repositories", { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
      .then(function (r) {
        if (loading) loading.hidden = true;
        if (r.status !== 200) {
          showError((r.data && r.data.error) || "Could not load repositories.");
          if (table) table.hidden = true;
          return;
        }
        var repos = r.data.repositories || [];
        if (table) {
          table.hidden = false;
          if (toolbar) toolbar.hidden = false;
        }
        if (empty) empty.hidden = repos.length > 0;
        tbody.innerHTML = "";
        repos.forEach(addRepoRow);
        renderSelection();
      })
      .catch(function () {
        if (loading) loading.hidden = true;
        showError("Could not reach the server.");
      });
  }

  function renderSelection() {
    var n = selected.size;
    if (selectedCount) {
      selectedCount.textContent = n === 0 ? "Nothing selected"
        : n === 1 ? "1 repository selected"
        : n + " repositories selected";
    }
    if (protectBtn) protectBtn.disabled = n === 0;
  }

  function addRepoRow(repo) {
    if (!tbody) return;
    var tr = el("tr");
    tr.setAttribute("data-id", String(repo.githubId));

    var box = el("input");
    box.type = "checkbox";
    box.className = "repo-select";
    box.setAttribute("data-id", String(repo.githubId));
    box.checked = !!repo.protected;
    if (repo.protected) { box.disabled = true; }
    box.addEventListener("change", function () {
      if (box.checked) { selected.add(String(repo.githubId)); } else { selected.delete(String(repo.githubId)); }
      renderSelection();
    });
    var tdSel = el("td");
    tdSel.appendChild(box);
    tr.appendChild(tdSel);

    var name = el("span", "repo-link", repo.fullName);
    if (repo.private) name.appendChild(el("span", "mono text-xs neutral-weak", " · private"));
    tr.appendChild(tdCell(name));
    tr.appendChild(tdCell(el("span", "mono", repo.defaultBranch || "—")));

    var status = repo.protected
      ? el("span", "status status--protected", "Protected")
      : el("span", "status status--stale", "Not protected");
    tr.appendChild(tdCell(status));
    tbody.appendChild(tr);
  }

  function tdCell(child) {
    var td = el("td");
    td.appendChild(child);
    return td;
  }

  function loadRepositories() {
    return fetch("/api/repositories", { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
      .then(function (r) {
        if (loading) loading.hidden = true;
        if (r.status !== 200) {
          showError((r.data && r.data.error) || "Could not load repositories.");
          if (table) table.hidden = true;
          return;
        }
        var repos = r.data.repositories || [];
        if (table) {
          table.hidden = false;
          if (toolbar) toolbar.hidden = false;
        }
        if (empty) empty.hidden = repos.length > 0;
        tbody.innerHTML = "";
        repos.forEach(addRepoRow);
        renderSelection();
      })
      .catch(function () {
        if (loading) loading.hidden = true;
        showError("Could not reach the server.");
      });
  }

  if (protectBtn) {
    protectBtn.addEventListener("click", function () {
      if (protectBtn.disabled) return;
      var ids = Array.from(selected).map(Number);
      if (ids.length === 0) return;
      protectBtn.disabled = true;
      fetch("/api/protected-repositories", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken },
        body: JSON.stringify({ repoIds: ids })
      })
        .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
        .then(function (r) {
          var data = r.data || {};
          if (r.status === 201) {
            var n = (data.protected || []).length;
            var dup = (data.alreadyProtected || []).length;
            selected.clear();
            flashMessage(
              (n > 0 ? n + " protected." : "") +
              (dup > 0 ? " " + dup + " already protected." : "") +
              (Object.keys(data.errors || {}).length > 0 ? " Some could not be protected." : ""),
              Object.keys(data.errors || {}).length > 0 ? "error" : "success");
            loadAll();
          } else {
            flashMessage((data.error) || "Could not protect repositories.", "error");
            protectBtn.disabled = false;
            renderSelection();
          }
        })
        .catch(function () {
          protectBtn.disabled = false;
          renderSelection();
          flashMessage("Could not reach the server.", "error");
        });
    });
  }

  if (backupAllBtn) {
    backupAllBtn.addEventListener("click", function () {
      if (backupAllBtn.disabled || protectedRepos.length === 0) return;
      backupAllBtn.disabled = true;
      backupFlashMessage("Starting backups for all protected repositories…", "success");
      var chain = Promise.resolve();
      protectedRepos.forEach(function (rp) {
        chain = chain.then(function () {
          return triggerBackup(rp.id, rp.fullName, null);
        });
      });
      chain.then(function () {
        backupAllBtn.disabled = false;
        refreshAfterBackup();
      });
    });
  }

  function retentionFlashMessage(text, kind) {
    if (!retFlash) return;
    retFlash.textContent = text;
    retFlash.className = "form__flash form__flash--" + (kind === "error" ? "error" : "success");
    retFlash.hidden = false;
    window.clearTimeout(retentionFlashMessage._t);
    retentionFlashMessage._t = window.setTimeout(function () { retFlash.hidden = true; }, 5000);
  }

  function renderCleanupResult(r) {
    if (!retResult) return;
    r = r || {};
    var parts = [];
    parts.push("Inspected " + (r.inspected || 0) + " records");
    var local = (r.localBundles || []).length;
    var orphans = r.orphanDeleted || 0;
    var drive = (r.driveFileIds || []).length;
    if (local > 0 || orphans > 0 || drive > 0) {
      parts.push("deleted " + local + " local bundle" + (local === 1 ? "" : "s") +
        (orphans > 0 ? " (" + orphans + " orphan" + (orphans === 1 ? "" : "s") + ")" : "") +
        (drive > 0 ? " and " + drive + " Drive cop" + (drive === 1 ? "y" : "ies") : ""));
    } else {
      parts.push("nothing to delete");
    }
    if (r.skipped > 0) { parts.push(r.skipped + " skipped (active/missing)"); }
    if (r.missing > 0) { parts.push(r.missing + " already missing"); }
    var text = parts.join(" · ");
    if ((r.errors || []).length > 0) {
      text += " · " + r.errors.length + " error(s)";
      retResult.className = "form__flash form__flash--error";
    } else {
      retResult.className = "text-sm neutral-weak";
    }
    retResult.textContent = text;
  }

  function loadRetention() {
    return fetch("/api/retention", { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
      .then(function (r) {
        var rt = (r.data && r.data.retention) || {};
        if (retKeepLocal) retKeepLocal.value = rt.keepLocal || 0;
        if (retKeepDays) retKeepDays.value = rt.keepLocalDays || 0;
        if (retKeepJobs) retKeepJobs.value = rt.keepJobs || 0;
        if (retKeepDriveDays) retKeepDriveDays.value = rt.keepDriveDays || 0;
        if (retDriveEnabled) retDriveEnabled.checked = !!rt.driveRetentionEnabled;
        if (retDriveHint) {
          retDriveHint.textContent = rt.driveConfigured
            ? "Requires drive copies enabled in settings."
            : "Drive is not configured; copies are stored locally only.";
        }
        if (retSchedEnabled) retSchedEnabled.checked = !!rt.scheduledCleanupEnabled;
        if (retSchedDays) retSchedDays.value = rt.scheduledCleanupIntervalDays || 0;
        renderScheduledStatus(r.data && r.data.scheduled);
        renderCleanupResult(r.data && r.data.lastCleanup);

        var not = (r.data && r.data.notifications) || {};
        if (notifEnabled) notifEnabled.checked = !!not.enabled;
        if (notifUrl) notifUrl.value = not.webhookUrl || "";
        if (notifTimeout) notifTimeout.value = not.webhookTimeoutSeconds || 10;
        // Auth is write-only: never pre-fill the header/token inputs. The hint
        // tells the operator that auth is stored so blank fields read as "keep".
        if (notifAuthHeader) notifAuthHeader.value = "";
        if (notifAuthToken) notifAuthToken.value = "";
        if (notifAuthHint) {
          notifAuthHint.hidden = !not.webhookAuthConfigured;
          notifAuthHint.textContent = not.webhookAuthConfigured
            ? "Auth is configured; leave blank to keep it."
            : "";
        }
        renderOverrides(r.data && r.data.overrides);
      })
      .catch(function () {});
  }

  function notifFlashMessage(text, kind) {
    if (!notifFlash) return;
    notifFlash.textContent = text;
    notifFlash.className = "form__flash form__flash--" + (kind === "error" ? "error" : "success");
    notifFlash.hidden = false;
    window.clearTimeout(notifFlashMessage._t);
    notifFlashMessage._t = window.setTimeout(function () { notifFlash.hidden = true; }, 6000);
  }

  function overrideRepoOptions(selected) {
    var opts = [];
    var taken = {};
    overrideList.querySelectorAll(".ov-repo").forEach(function (sel) {
      if (Number(sel.value)) taken[Number(sel.value)] = true;
    });
    (protectedRepos || []).forEach(function (rp) {
      var opt = document.createElement("option");
      opt.value = String(rp.githubId);
      opt.textContent = rp.fullName;
      if (Number(selected) === Number(rp.githubId)) opt.selected = true;
      opts.push(opt);
    });
    return opts;
  }

  function appendOverrideRow(rp, ov, locked) {
    if (!overrideList) return;
    ov = ov || {};
    var row = el("div", "override-row");
    row.setAttribute("style", "display:flex;align-items:center;gap:0.5rem;margin-bottom:0.5rem;flex-wrap:wrap");

    var select = el("select", "field__input");
    select.className = "ov-repo";
    select.style.width = "12rem";
    if (locked) { select.disabled = true; }
    overrideRepoOptions(Number(rp.githubId)).forEach(function (o) { select.appendChild(o); });
    row.appendChild(select);

    var fields = [
      { cls: "ov-keepLocal", placeholder: "copies", value: ov.keepLocal },
      { cls: "ov-keepDays", placeholder: "days", value: ov.keepLocalDays },
      { cls: "ov-keepJobs", placeholder: "jobs", value: ov.keepJobs },
      { cls: "ov-keepDriveDays", placeholder: "drive days", value: ov.keepDriveDays }
    ];
    fields.forEach(function (f) {
      var input = el("input", "field__input");
      input.type = "number";
      input.min = "0";
      input.className += " " + f.cls;
      input.placeholder = f.placeholder;
      input.style.width = "5rem";
      input.title = f.placeholder;
      if (f.value !== undefined && f.value !== null) input.value = f.value;
      row.appendChild(input);
    });

    var driveBox = el("input");
    driveBox.type = "checkbox";
    driveBox.checked = !!ov.driveRetentionEnabled;
    driveBox.className = "ov-drive";
    var driveLabel = el("label", "field__label");
    driveLabel.appendChild(driveBox);
    driveLabel.appendChild(document.createTextNode(" Drive"));
    row.appendChild(driveLabel);

    var removeBtn = el("button", "btn btn--ghost", "Remove");
    removeBtn.type = "button";
    removeBtn.style.fontSize = "0.8rem";
    removeBtn.addEventListener("click", function () {
      row.remove();
      refreshOverrideRepoOptions();
    });
    row.appendChild(removeBtn);
    overrideList.appendChild(row);
    refreshOverrideRepoOptions();
  }

  function refreshOverrideRepoOptions() {
    // Recompute the "Add" button's default and disable selects whose repo was
    // taken by another row. Locked rows keep their repo regardless.
    var taken = {};
    overrideList.querySelectorAll(".ov-repo").forEach(function (sel) {
      if (!sel.disabled && Number(sel.value)) taken[Number(sel.value)] = true;
    });
    if (overrideAddBtn) overrideAddBtn.disabled = (protectedRepos || []).length === 0;
  }

  function renderOverrides(overrides) {
    if (!overrideList) return;
    overrideList.innerHTML = "";
    var byRepo = {};
    (overrides || []).forEach(function (ov) { byRepo[String(ov.repositoryId)] = ov; });
    var shown = (protectedRepos || []).filter(function (rp) {
      return byRepo.hasOwnProperty(String(rp.githubId));
    });
    if (shown.length === 0) {
      overrideList.appendChild(el("div", "text-sm neutral-weak", "No overrides yet. Add one to tune a specific repository."));
    }
    shown.forEach(function (rp) { appendOverrideRow(rp, byRepo[String(rp.githubId)], true); });
    refreshOverrideRepoOptions();
  }

  function collectOverrides() {
    if (!overrideList) return [];
    var rows = [];
    var seen = {};
    overrideList.querySelectorAll(".override-row").forEach(function (row) {
      var repoId = Number(row.querySelector(".ov-repo").value);
      if (!repoId || seen[repoId]) return;
      seen[repoId] = true;
      var ov = { repositoryId: repoId };
      [["ov-keepLocal", "keepLocal"], ["ov-keepDays", "keepLocalDays"], ["ov-keepJobs", "keepJobs"], ["ov-keepDriveDays", "keepDriveDays"]].forEach(function (pair) {
        var v = parseInt(row.getElementsByClassName(pair[0])[0].value, 10);
        if (!isNaN(v)) ov[pair[1]] = v;
      });
      var driveBox = row.getElementsByClassName("ov-drive")[0];
      if (driveBox && driveBox.checked) ov.driveRetentionEnabled = true;
      rows.push(ov);
    });
    return rows;
  }

  function collectNotifications() {
    var n = {};
    if (notifEnabled) n.enabled = !!notifEnabled.checked;
    if (notifUrl) n.webhookUrl = notifUrl.value.trim();
    if (notifAuthHeader && notifAuthHeader.value.trim()) n.webhookAuthHeader = notifAuthHeader.value.trim();
    if (notifAuthToken && notifAuthToken.value) n.webhookAuthToken = notifAuthToken.value;
    if (notifTimeout) n.webhookTimeoutSeconds = Math.max(1, parseInt(notifTimeout.value, 10) || 10);
    return n;
  }

  function renderScheduledStatus(s) {
    if (!retSchedStatus) return;
    s = s || {};
    if (!s.enabled) {
      retSchedStatus.textContent = "";
      retSchedStatus.className = "text-sm neutral-weak";
      return;
    }
    var parts = [];
    if (s.running) {
      parts.push("Running now");
    } else if (s.intervalDays > 0) {
      parts.push("Every " + s.intervalDays + " day" + (s.intervalDays === 1 ? "" : "s"));
    }
    if (s.lastTrigger) {
      var outcome = "";
      if (s.success === true) outcome = " · ok";
      else if (s.success === false) outcome = " · errors";
      parts.push("Last run: " + s.lastTrigger + outcome + (s.lastFinish ? " at " + s.lastFinish : ""));
    }
    if (!s.running && s.lastTrigger && s.nextRun) {
      parts.push("Next: " + s.nextRun);
    }
    if (s.lastResult && (s.lastResult.errors || []).length > 0) {
      parts.push((s.lastResult.errors.length) + " error(s) in last run");
    }
    retSchedStatus.textContent = parts.join(" · ");
    retSchedStatus.className = s.running ? "text-sm status status--progress" : "text-sm neutral-weak";
  }

  if (retSaveBtn) {
    retSaveBtn.addEventListener("click", function () {
      retSaveBtn.disabled = true;
      fetch("/api/retention", {
        method: "PUT",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken },
        body: JSON.stringify({
          keepLocal: num(retKeepLocal),
          keepLocalDays: num(retKeepDays),
          keepJobs: num(retKeepJobs),
          keepDriveDays: num(retKeepDriveDays),
          driveRetentionEnabled: !!(retDriveEnabled && retDriveEnabled.checked),
          scheduledCleanupEnabled: !!(retSchedEnabled && retSchedEnabled.checked),
          scheduledCleanupIntervalDays: num(retSchedDays),
          notifications: collectNotifications(),
          overrides: collectOverrides()
        })
      })
        .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
        .then(function (r) {
          retSaveBtn.disabled = false;
          if (r.status === 200) {
            retentionFlashMessage("Retention settings saved.", "success");
            loadRetention();
          } else {
            retentionFlashMessage((r.data && r.data.error) || "Could not save retention settings.", "error");
          }
        })
        .catch(function () {
          retSaveBtn.disabled = false;
          retentionFlashMessage("Could not reach the server.", "error");
        });
    });
  }

  if (retRunBtn) {
    retRunBtn.addEventListener("click", function () {
      retRunBtn.disabled = true;
      fetch("/api/retention/cleanup", {
        method: "POST",
        headers: { "X-CSRF-Token": csrfToken }
      })
        .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
        .then(function (r) {
          retRunBtn.disabled = false;
          if (r.status === 200) {
            renderCleanupResult(r.data && r.data.cleanup);
            retentionFlashMessage("Cleanup complete.", "success");
            loadHistory();
            loadRetention();
          } else {
            retentionFlashMessage((r.data && r.data.error) || "Could not run cleanup.", "error");
          }
        })
        .catch(function () {
          retRunBtn.disabled = false;
          retentionFlashMessage("Could not reach the server.", "error");
        });
    });
  }

  if (notifTestBtn) {
    notifTestBtn.addEventListener("click", function () {
      notifTestBtn.disabled = true;
      fetch("/api/retention/notify-test", {
        method: "POST",
        headers: { "X-CSRF-Token": csrfToken }
      })
        .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
        .then(function (r) {
          notifTestBtn.disabled = false;
          if (r.status === 200) {
            notifFlashMessage((r.data && r.data.message) || "Test notification delivered.", "success");
          } else {
            notifFlashMessage((r.data && r.data.error) || "Test notification failed.", "error");
          }
        })
        .catch(function () {
          notifTestBtn.disabled = false;
          notifFlashMessage("Could not reach the server.", "error");
        });
    });
  }

  if (overrideAddBtn) {
    overrideAddBtn.addEventListener("click", function () {
      if (overrideAddBtn.disabled) return;
      var pool = (protectedRepos || []).filter(function (rp) {
        var taken = false;
        overrideList.querySelectorAll(".ov-repo").forEach(function (sel) {
          if (!sel.disabled && Number(sel.value) === Number(rp.githubId)) taken = true;
        });
        return !taken;
      });
      if (pool.length === 0) return;
      appendOverrideRow(pool[0], {}, false);
    });
  }

  function num(input) {
    var n = input ? parseInt(input.value, 10) : NaN;
    return isNaN(n) || n < 0 ? 0 : n;
  }

  // Fetch a CSRF token first (this also establishes the session cookie), then
  // load the repositories and current protection state.
  fetch("/api/csrf", { headers: { "Accept": "application/json" } })
    .then(function (resp) { return resp.json(); })
    .then(function (data) { csrfToken = data.csrfToken || ""; return loadAll().then(loadRetention); })
    .catch(function () { loadAll().then(loadRetention); });
})();
