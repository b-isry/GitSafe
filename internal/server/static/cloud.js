(function () {
  "use strict";

  var root = document.getElementById("cloudRepos");
  if (!root) return; // not the connected cloud dashboard

  var loading = document.getElementById("cloudLoading");
  var errorBox = document.getElementById("cloudError");
  var errorText = document.getElementById("cloudErrorText");
  var retryBtn = document.getElementById("cloudRetryBtn");
  var reconnectPanel = document.getElementById("reconnectPanel");
  var reconnectText = document.getElementById("reconnectText");
  var repoTable = document.getElementById("repoTable");
  var repoRows = document.getElementById("repoRows");
  var repoEmpty = document.getElementById("repoEmpty");
  var repoEmptyTitle = document.getElementById("repoEmptyTitle");
  var repoEmptyBody = document.getElementById("repoEmptyBody");
  var repoCount = document.getElementById("repoCount");
  var flash = document.getElementById("flash");
  var backupAllBtn = document.getElementById("backupAllBtn");
  var activityList = document.getElementById("backupActivityList");
  var activityEmpty = document.getElementById("backupActivityEmpty");

  // GitHub and Drive pills in the top navigation bar (only present when
  // connected, matching the server-rendered layout).
  var githubPill = document.getElementById("githubPill") || null;
  var drivePill = document.getElementById("drivePill") || null;
  var drivePillLabel = document.getElementById("drivePillLabel");
  var drivePillDot = document.getElementById("drivePillDot");
  var drivePillConnect = document.getElementById("drivePillConnect");
  var drivePillDisconnect = document.getElementById("drivePillDisconnect");

  var csrfToken = "";
  var repos = [];            // discovered repositories w/ backup status from the API
  var filter = "all";        // all | backedup | notbackedup
  var activityByID = {};     // job id -> live activity row element

  var MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }

  function flashMessage(text, kind) {
    if (!flash) return;
    flash.textContent = text;
    flash.className = "form__flash form__flash--" + (kind === "error" ? "error" : "success");
    flash.hidden = false;
    window.clearTimeout(flashMessage._t);
    flashMessage._t = window.setTimeout(function () { flash.hidden = true; }, 5000);
  }

  function clearRepoError() {
    if (errorBox) errorBox.hidden = true;
    if (errorText) errorText.textContent = "";
  }

  function showRepoError(msg) {
    if (errorText) errorText.textContent = msg;
    if (errorBox) errorBox.hidden = false;
  }

  function showReconnect(msg) {
    if (loading) loading.hidden = true;
    if (reconnectText) reconnectText.textContent = msg;
    if (reconnectPanel) reconnectPanel.hidden = false;
    if (repoTable) repoTable.hidden = true;
    if (repoEmpty) repoEmpty.hidden = true;
    clearRepoError();
  }

  function hideReconnect() {
    if (reconnectPanel) reconnectPanel.hidden = true;
  }

  // --- repository list ---

  function loadRepositories() {
    if (loading) loading.hidden = false;
    return fetch("/api/repositories", { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
      .then(function (r) {
        if (loading) loading.hidden = true;
        if (r.status === 401 || r.status === 409) {
          var msg = (r.data && r.data.error) || "Your GitHub authorization is no longer valid. Reconnect your account to continue.";
          showReconnect(msg);
          return;
        }
        if (r.status !== 200) {
          hideReconnect();
          var reason = (r.data && r.data.error) || "Could not reach GitHub. Please try again later.";
          showRepoError("Connected, but couldn't load your repositories \u2014 " + reason);
          return;
        }
        clearRepoError();
        hideReconnect();
        repos = r.data.repositories || [];
        render();
      })
      .catch(function () {
        if (loading) loading.hidden = true;
        hideReconnect();
        showRepoError("Connected, but couldn't load your repositories \u2014 could not reach the server.");
      });
  }

  function render() {
    if (repoCount) repoCount.textContent = String(repos.length);
    renderRows();
    if (backupAllBtn) backupAllBtn.disabled = repos.length === 0;
  }

  function visibleRepos() {
    if (filter === "backedup") return repos.filter(function (r) { return r.backedUp; });
    if (filter === "notbackedup") return repos.filter(function (r) { return !r.backedUp; });
    return repos.slice();
  }

  function renderRows() {
    if (!repoRows) return;
    var list = visibleRepos();
    repoRows.innerHTML = "";
    list.forEach(function (repo) { repoRows.appendChild(repoRow(repo)); });

    var showTable = list.length > 0;
    if (repoTable) repoTable.hidden = !showTable;
    if (repoEmpty) {
      repoEmpty.hidden = showTable;
      if (!showTable) {
        if (repoEmptyTitle) repoEmptyTitle.textContent = emptyTitle();
        if (repoEmptyBody) repoEmptyBody.textContent = emptyBody();
      }
    }
  }

  function emptyTitle() {
    if (repos.length === 0) return "No repositories";
    if (filter === "backedup") return "No backed up repositories";
    return "No repositories to back up";
  }

  function emptyBody() {
    if (repos.length === 0) return "No GitHub repositories were found for this account.";
    if (filter === "backedup") return "Run a backup and it will appear under Backed up.";
    return "Every repository on this account has been backed up.";
  }

  // Each cell builder returns the full column element it owns; the list code
  // appends elements directly to the row. Under no circumstances is an element
  // coerced to a string (that is what rendered "[object HTMLSpanElement]").
  function repoRow(repo) {
    var tr = el("div", "repo-row");
    tr.setAttribute("data-github-id", String(repo.githubId));
    tr.appendChild(nameCell(repo));
    tr.appendChild(activityCell(repo));
    tr.appendChild(statusCell(repo));
    tr.appendChild(latestBackupCell(repo));
    tr.appendChild(actionsCell(repo));
    return tr;
  }

  function nameCell(repo) {
    var wrap = el("div", "repo-col repo-col--name");
    wrap.appendChild(el("div", "repo-name", repo.fullName));
    var meta = el("div", "repo-meta");
    meta.appendChild(el("span", "repo-branch", repo.defaultBranch || "—"));
    meta.appendChild(el("span", "repo-vis " + (repo.private ? "repo-vis--private" : "repo-vis--public"), repo.private ? "private" : "public"));
    wrap.appendChild(meta);
    return wrap;
  }

  function activityCell(repo) {
    var wrap = el("div", "repo-col repo-col--activity");
    if (!repo.updatedAt || /^0001-01-01/.test(repo.updatedAt)) {
      wrap.appendChild(el("span", "repo-activity", "Never updated"));
    } else {
      wrap.appendChild(el("span", "repo-activity", "Updated " + relativeTime(repo.updatedAt)));
    }
    return wrap;
  }

  function statusCell(repo) {
    var wrap = el("div", "repo-col repo-col--status");
    var pill = el("span", repo.backedUp ? "backup-pill backup-pill--on" : "backup-pill");
    pill.appendChild(el("span", "backup-pill__dot"));
    pill.appendChild(document.createTextNode(repo.backedUp ? "Backed up" : "Not backed up"));
    wrap.appendChild(pill);
    return wrap;
  }

  function latestBackupCell(repo) {
    var wrap = el("div", "repo-col repo-col--backup");
    var lb = repo.latestBackup;
    if (!lb) {
      wrap.appendChild(el("span", "repo-backup__text repo-backup__text--muted", "Not backed up"));
      return wrap;
    }
    var line = el("span", "repo-backup__text", niceTimestamp(lb.createdAt) + " \u00b7 " + humanBytes(lb.bundleSizeBytes));
    wrap.appendChild(line);
    return wrap;
  }

  function actionsCell(repo) {
    var wrap = el("div", "repo-col repo-col--actions repo-actions");
    if (repo.backedUp) {
      var stack = el("div", "repo-actions__stack");
      if (repo.latestBackup && repo.latestBackup.driveViewLink) {
        var link = document.createElement("a");
        link.className = "btn btn--primary";
        link.href = repo.latestBackup.driveViewLink;
        link.target = "_blank";
        link.rel = "noopener";
        link.textContent = "View in Drive";
        stack.appendChild(link);
      }
      var backupNow = el("button", "btn", "Backup Now");
      backupNow.type = "button";
      backupNow.addEventListener("click", function () { backupRepo(repo, backupNow); });
      stack.appendChild(backupNow);
      wrap.appendChild(stack);
      return wrap;
    }
    var backupBtn = el("button", "btn btn--primary", "Back up");
    backupBtn.type = "button";
    backupBtn.addEventListener("click", function () { backupRepo(repo, backupBtn); });
    wrap.appendChild(backupBtn);
    return wrap;
  }

  // --- backups ---

  // pollJob resolves with the job once it reaches a terminal state. The
  // optional onPoll callback runs after every poll for live progress output.
  function pollJob(id, onPoll) {
    return fetch("/api/backup-jobs/" + encodeURIComponent(id), { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.json(); })
      .then(function (job) {
        if (onPoll) onPoll(job);
        if (job.state === "completed" || job.state === "failed" || job.state === "interrupted") {
          return job;
        }
        return new Promise(function (resolve) {
          window.setTimeout(function () { resolve(pollJob(id, onPoll)); }, 800);
        });
      });
  }

  // startBackup posts a backup-start request and streams its job to the
  // activity card, returning the finished job (or null when it could not
  // start). The endpoint used is the direct, protection-free one.
  function startBackup(path, fullName) {
    return fetch(path, {
      method: "POST",
      headers: { "X-CSRF-Token": csrfToken }
    })
      .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
      .then(function (r) {
        if (r.status !== 202) {
          flashMessage((r.data && r.data.error) || "Could not start the backup.", "error");
          return null;
        }
        return pollJob(r.data.id, function (job) { upsertBackupActivity(job); }).then(function (job) {
          if (job.state === "failed") {
            flashMessage("Backup failed for " + fullName + (job.error ? ": " + job.error : ""), "error");
          } else if (job.state === "completed") {
            flashMessage("Backup complete for " + fullName + ".", "success");
          }
          return job;
        });
      })
      .catch(function () {
        flashMessage("Could not reach the server.", "error");
        return null;
      });
  }

  function backupRepo(repo, btn) {
    if (btn) { btn.disabled = true; }
    startBackup("/api/repositories/" + encodeURIComponent(String(repo.githubId)) + "/backup", repo.fullName)
      .then(function (job) {
        if (btn) { btn.disabled = false; }
        if (job && job.state === "completed") {
          loadRepositories();
        } else if (job) {
          loadRepositories(); // failed attempt must still leave "Not backed up"
        }
      });
  }

  function backupAll() {
    if (backupAllBtn) { backupAllBtn.disabled = true; }
    startBackupAll().then(function () {
      if (backupAllBtn) { backupAllBtn.disabled = repos.length === 0; }
    });
  }

  function startBackupAll() {
    return fetch("/api/backups", {
      method: "POST",
      headers: { "X-CSRF-Token": csrfToken }
    })
      .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
      .then(function (r) {
        if (r.status !== 202) {
          flashMessage((r.data && r.data.error) || "Could not start the backups.", "error");
          return;
        }
        var jobs = (r.data && r.data.jobs) || [];
        if (!jobs.length) {
          flashMessage("Nothing to back up.", "success");
          return;
        }
        flashMessage("Starting " + jobs.length + " backup" + (jobs.length === 1 ? "" : "s") + "\u2026", "success");
        return Promise.all(jobs.map(function (j) {
          return pollJob(j.id, function (job) { upsertBackupActivity(job); });
        })).then(function (finished) {
          loadRepositories();
          var failed = finished.filter(function (j) { return j.state === "failed"; }).length;
          if (failed) {
            flashMessage(failed + " backup" + (failed === 1 ? "" : "s") + " failed.", "error");
          } else {
            flashMessage("All backups complete.", "success");
          }
        });
      })
      .catch(function () {
        flashMessage("Could not reach the server.", "error");
      });
  }

  if (backupAllBtn) {
    backupAllBtn.addEventListener("click", backupAll);
  }

  // --- backup activity card ---

  // jobLabel maps a job state machine value to a short user-facing phase.
  function jobLabel(state) {
    var map = {
      enqueued: "Preparing", authorizing: "Authorizing", cloning: "Bundling",
      bundling: "Bundling", validating: "Bundling", recording: "Finalizing",
      uploading: "Uploading", completed: "Completed", failed: "Backup failed",
      interrupted: "Interrupted"
    };
    return map[state] || state;
  }

  function syncActivityEmpty() {
    if (activityEmpty) activityEmpty.hidden = activityList.children.length > 0;
  }

  function upsertBackupActivity(job) {
    if (!activityList) return;
    var row = activityByID[job.id];
    if (!row) {
      row = document.createElement("div");
      row.className = "backup-activity__row";
      row.appendChild(el("div", "backup-activity__head"));
      row.appendChild(el("div", "backup-activity__track"));
      activityList.appendChild(row);
      activityByID[job.id] = row;
      syncActivityEmpty();
    }
    renderActivityRow(row, job);
    if (job.state === "completed" || job.state === "failed" || job.state === "interrupted") {
      window.setTimeout(function () { removeBackupActivity(job.id); }, 4000);
    }
  }

  function removeBackupActivity(id) {
    var row = activityByID[id];
    if (!row || !row.parentNode) return;
    delete activityByID[id];
    row.remove();
    syncActivityEmpty();
  }

  function renderActivityRow(row, job) {
    var head = row.querySelector(".backup-activity__head");
    var track = row.querySelector(".backup-activity__track");
    if (!head || !track) return;

    head.innerHTML = "";
    var label = jobLabel(job.state);
    var chip = el("span", "status status--progress", label);
    if (job.state === "failed") chip.className = "status status--failed";
    if (job.state === "interrupted") chip.className = "status status--stale";
    head.appendChild(el("span", "repo-link", job.fullName || "Backup"));
    head.appendChild(chip);
    if (job.state === "uploading") {
      var pct = Math.max(0, Math.min(100, Number(job.progress) || 0));
      head.appendChild(el("span", "mono text-xs neutral-weak",
        pct + "% \u00b7 " + humanBytes(job.uploadedBytes) + " of " + humanBytes(job.totalBytes)));
    }
    head.appendChild(el("span", "backup-activity__spacer"));

    track.innerHTML = "";
    var bar = el("div", "backup-activity__bar");
    if (job.state === "uploading") {
      bar.style.width = (Math.max(0, Math.min(100, Number(job.progress) || 0))) + "%";
    } else {
      bar.className += " backup-activity__bar--indeterminate";
    }
    track.appendChild(bar);
  }

  // --- helpers ---

  function humanBytes(n) {
    if (!n && n !== 0) return "\u2014";
    var u = ["B", "KB", "MB", "GB"];
    var i = 0;
    var v = Number(n);
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return v.toFixed(i === 0 ? 0 : 1) + " " + u[i];
  }

  function relativeTime(ts) {
    if (!ts) return "";
    var then = new Date(ts).getTime();
    if (isNaN(then)) return "";
    var secs = Math.floor((Date.now() - then) / 1000);
    if (secs < 0) secs = 0;
    if (secs < 60) return "just now";
    var mins = Math.floor(secs / 60);
    if (mins < 60) return mins + "m ago";
    var hours = Math.floor(mins / 60);
    if (hours < 24) return hours + "h ago";
    var days = Math.floor(hours / 24);
    if (days < 30) return days + "d ago";
    var months = Math.floor(days / 30);
    if (months < 12) return months + "mo ago";
    return Math.floor(months / 12) + "y ago";
  }

  // niceTimestamp formats a backup timestamp like "Today, 9:32 AM",
  // "Yesterday, 5:12 PM", "Sep 12, 9:32 AM" or "Sep 12, 2025, 9:32 AM".
  function niceTimestamp(ts) {
    if (!ts) return "";
    var d = new Date(ts);
    if (isNaN(d.getTime())) return "";
    var now = new Date();
    var sameDay = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
    var yesterday = new Date(now);
    yesterday.setDate(now.getDate() - 1);
    var isYesterday = d.getFullYear() === yesterday.getFullYear() && d.getMonth() === yesterday.getMonth() && d.getDate() === yesterday.getDate();

    var h = d.getHours();
    var ampm = h >= 12 ? "PM" : "AM";
    var h12 = h % 12;
    if (h12 === 0) h12 = 12;
    var mm = ("0" + d.getMinutes()).slice(-2);
    var time = h12 + ":" + mm + " " + ampm;

    if (sameDay) return "Today, " + time;
    if (isYesterday) return "Yesterday, " + time;
    var datePart = MONTHS[d.getMonth()] + " " + d.getDate();
    if (d.getFullYear() !== now.getFullYear()) datePart += ", " + d.getFullYear();
    return datePart + ", " + time;
  }

  // --- filters ---

  var filterBtns = document.querySelectorAll(".repo-filters .filter");
  filterBtns.forEach(function (btn) {
    btn.addEventListener("click", function () {
      filter = btn.getAttribute("data-filter");
      filterBtns.forEach(function (b) { b.classList.toggle("is-active", b === btn); });
      renderRows();
    });
  });

  // --- connection pills (top navigation bar) ---

  if (githubPill) {
    githubPill.querySelector(".conn-pill__btn").addEventListener("click", function (e) {
      e.stopPropagation();
      closeMenus();
      githubPill.classList.add("is-open");
    });
  }
  if (drivePill) {
    drivePill.querySelector(".conn-pill__btn").addEventListener("click", function (e) {
      e.stopPropagation();
      closeMenus();
      drivePill.classList.add("is-open");
    });
  }
  document.addEventListener("click", closeMenus);
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") closeMenus();
  });

  function closeMenus() {
    document.querySelectorAll(".conn-pill.is-open").forEach(function (m) {
      m.classList.remove("is-open");
    });
  }

  document.querySelectorAll("[data-disconnect]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      disconnectConnection(btn.getAttribute("data-disconnect"), btn);
    });
  });

  function disconnectConnection(which, btn) {
    if (btn) { btn.disabled = true; }
    fetch("/api/connections/" + encodeURIComponent(which), {
      method: "DELETE",
      headers: { "X-CSRF-Token": csrfToken }
    })
      .then(function (resp) { return resp.json().then(function (d) { return { status: resp.status, data: d }; }); })
      .then(function (r) {
        if (r.status === 200) {
          window.location.reload();
          return;
        }
        if (btn) btn.disabled = false;
        flashMessage((r.data && r.data.message) || "Could not disconnect.", "error");
      })
      .catch(function () {
        if (btn) btn.disabled = false;
        flashMessage("Could not reach the server.", "error");
      });
  }

  // loadDrivePill reflects the Google Drive connection state in its topbar
  // pill (email + green dot when connected, otherwise a neutral pill), resolved
  // from /api/connections.
  function loadDrivePill() {
    if (!drivePill) return;
    fetch("/api/connections", { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.json(); })
      .then(function (data) {
        var d = (data && data.drive) || {};
        if (d.connected) {
          if (drivePillLabel) drivePillLabel.textContent = d.accountEmail ? d.accountEmail : "Drive connected";
          if (drivePillDot) drivePillDot.classList.add("conn-pill__dot--on");
          if (drivePillConnect) drivePillConnect.hidden = true;
          if (drivePillDisconnect) drivePillDisconnect.hidden = false;
        } else if (d.configured) {
          if (drivePillLabel) drivePillLabel.textContent = "Drive \u00b7 Not connected";
          if (drivePillDot) drivePillDot.classList.remove("conn-pill__dot--on");
          if (drivePillConnect) drivePillConnect.hidden = false;
          if (drivePillDisconnect) drivePillDisconnect.hidden = true;
        } else {
          if (drivePillLabel) drivePillLabel.textContent = "Drive \u00b7 Not enabled";
          if (drivePillDot) drivePillDot.classList.remove("conn-pill__dot--on");
          if (drivePillConnect) drivePillConnect.hidden = true;
          if (drivePillDisconnect) drivePillDisconnect.hidden = true;
        }
      })
      .catch(function () {
        if (drivePillLabel) drivePillLabel.textContent = "Drive \u00b7 Unavailable";
      });
  }

  // --- boot ---

  if (retryBtn) {
    retryBtn.addEventListener("click", function () {
      retryBtn.disabled = true;
      clearRepoError();
      if (loading) loading.hidden = false;
      if (repoTable) repoTable.hidden = true;
      loadRepositories().then(function () {
        if (retryBtn) retryBtn.disabled = false;
      }).catch(function () {
        if (retryBtn) retryBtn.disabled = false;
      });
    });
  }

  // Fetch a CSRF token first (this also establishes the session cookie), then
  // load the repositories and the connection pills.
  fetch("/api/csrf", { headers: { "Accept": "application/json" } })
    .then(function (resp) { return resp.json(); })
    .then(function (data) { csrfToken = data.csrfToken || ""; })
    .catch(function () { /* mutation endpoints will be blocked without a token */ })
    .then(function () { return loadRepositories(); })
    .then(loadDrivePill);
})();