(function () {
  "use strict";

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }

  function esc(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }

  var active = null;

  // FolderBrowser is a server-side directory picker. It only ever lists
  // directory names from /api/folders; file names and contents are never
  // exposed, and choosing a folder hands the absolute path back to the page.
  function FolderBrowser(opts) {
    this.opts = opts || {};
    this.path = this.opts.startPath || "";
    this.parent = "";
    this.isTop = false;
    this.build();
  }

  FolderBrowser.prototype.build = function () {
    var self = this;
    if (active) active.close();

    var scrim = el("div", "modal-scrim");
    scrim.setAttribute("role", "presentation");

    var modal = el("div", "modal");
    modal.setAttribute("role", "dialog");
    modal.setAttribute("aria-modal", "true");
    modal.setAttribute("aria-labelledby", "folderModalTitle");

    var head = el("div", "modal__head");
    head.appendChild(el("div", "text-md", this.opts.title || "Choose a folder"));
    this.pathLine = el("div", "mono text-xs neutral-weak modal__path", "");
    head.appendChild(this.pathLine);

    var body = el("div", "modal__body");
    var navRow = el("div", "folder-nav");
    this.upBtn = el("button", "btn btn--ghost", "Up one level");
    this.upBtn.type = "button";
    this.upBtn.addEventListener("click", function () {
      self.path = self.parent;
      self.load();
    });
    this.upBtn.disabled = true;
    var navSpacer = el("span", "folder-nav__spacer");
    navRow.appendChild(this.upBtn);
    navRow.appendChild(navSpacer);
    this.reloadBtn = el("button", "btn btn--ghost", "Refresh");
    this.reloadBtn.type = "button";
    this.reloadBtn.addEventListener("click", function () { self.load(); });
    navRow.appendChild(this.reloadBtn);

    this.list = el("ul", "folder-list");
    this.empty = el("div", "empty");
    this.empty.hidden = true;
    this.empty.appendChild(el("p", "empty__title", "This folder is empty"));
    this.error = el("div", "form__flash form__flash--error folder-error");
    this.error.hidden = true;

    body.appendChild(navRow);
    body.appendChild(this.list);
    body.appendChild(this.empty);
    body.appendChild(this.error);

    var foot = el("div", "modal__foot");
    var cancelBtn = el("button", "btn", "Cancel");
    cancelBtn.type = "button";
    cancelBtn.addEventListener("click", function () { self.close(); });
    this.selectBtn = el("button", "btn btn--primary", "Select this folder");
    this.selectBtn.type = "button";
    this.selectBtn.addEventListener("click", function () {
      var p = self.path;
      self.close();
      if (self.opts.onSelect && p) self.opts.onSelect(p);
    });

    foot.appendChild(cancelBtn);
    foot.appendChild(this.selectBtn);

    modal.appendChild(head);
    modal.appendChild(body);
    modal.appendChild(foot);
    scrim.appendChild(modal);

    function onKey(ev) {
      if (ev.key === "Escape") { ev.preventDefault(); self.close(); }
    }
    document.addEventListener("keydown", onKey);
    this._onKey = onKey;

    document.body.appendChild(scrim);
    this.scrim = scrim;
    this.selectBtn.focus();
    this.load();
  };

  FolderBrowser.prototype.close = function () {
    if (this.scrim && this.scrim.parentNode) {
      this.scrim.parentNode.removeChild(this.scrim);
    }
    if (this._onKey) document.removeEventListener("keydown", this._onKey);
    if (active === this) active = null;
  };

  FolderBrowser.prototype.load = function () {
    var self = this;
    self.error.hidden = true;
    self.list.innerHTML = "";

    fetch("/api/folders?path=" + encodeURIComponent(self.path))
      .then(function (r) {
        if (!r.ok) throw new Error("request failed");
        return r.json();
      })
      .then(function (res) {
        self.render(res);
      })
      .catch(function () {
        self.error.textContent = "Could not browse folders. Is the server running?";
        self.error.hidden = false;
      });
  };

  FolderBrowser.prototype.render = function (res) {
    var self = this;
    self.parent = res.parent || "";
    self.isTop = !!res.isTop;
    self.upBtn.disabled = !res.parent;
    self.selectBtn.disabled = self.path === "" || !!res.error;
    self.pathLine.textContent = self.path || (res.isTop ? "Choose a drive" : "");
    if (res.path) self.path = res.path;

    if (res.error) {
      self.error.textContent = res.error;
      self.error.hidden = false;
    }

    self.list.innerHTML = "";
    res.entries.forEach(function (entry) {
      var li = el("li", "folder-item");
      li.setAttribute("role", "button");
      li.setAttribute("tabindex", "0");
      li.appendChild(el("div", "folder-item__name", entry.name));
      li.appendChild(el("div", "mono folder-item__path", entry.path));

      function select() {
        self.path = entry.path;
        self.load();
      }
      li.addEventListener("click", select);
      li.addEventListener("keydown", function (ev) {
        if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); select(); }
      });
      self.list.appendChild(li);
    });

    self.empty.hidden = res.entries.length > 0 || !!res.error;
  };

  window.GitSafeFolderBrowser = {
    open: function (opts) {
      active = new FolderBrowser(opts || {});
    }
  };
})();