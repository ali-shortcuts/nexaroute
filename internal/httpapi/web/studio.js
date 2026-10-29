// NexaFlow Visual Studio: a standalone editor on the canonical admin API.
// It reads node schemas from GET /admin/api/nexaflow/nodes and keeps no
// duplicate frontend schema: field keys are the registry's own JSON
// (Name/Type/Required/Description, no lowercase tags on Field).
// It never sends a tenant id: scope comes from the server-side identity.
// It never asks for secret values: credential fields hold references only.
// Run views show server-redacted outputs and error codes only.
(function () {
  'use strict';
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };
  function esc(v) {
    return String(v === undefined || v === null ? '' : v)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }
  function showError(msg, retry) {
    var el = $('#studio-error');
    el.hidden = false;
    el.innerHTML = esc(msg) + (retry ? ' <button class="button secondary" id="error-retry" type="button">Retry</button>' : '');
    var b = $('#error-retry');
    if (b) b.addEventListener('click', retry);
  }
  function clearError() { var el = $('#studio-error'); el.hidden = true; el.innerHTML = ''; }

  // ---- auth + api (mirrors the dashboard: x-admin-key + CSRF) ----
  var adminKey = sessionStorage.getItem('nexaflow_studio_key') || '';
  var csrfToken = '';
  function api(path, opt) {
    opt = opt || {};
    var headers = { 'Accept': 'application/json' };
    if (opt.headers) for (var k in opt.headers) headers[k] = opt.headers[k];
    if (adminKey) headers['x-admin-key'] = adminKey;
    var method = String(opt.method || 'GET').toUpperCase();
    function doFetch() {
      return fetch(path, {
        method: method, headers: headers,
        body: opt.body === undefined ? undefined : JSON.stringify(opt.body),
        credentials: 'same-origin', cache: 'no-store'
      }).then(function (r) {
        return r.text().then(function (t) {
          var data = {};
          try { data = t ? JSON.parse(t) : {}; } catch (e) { /* non-JSON error body */ }
          if (!r.ok) {
            var err = new Error(data.message || data.error || ('HTTP ' + r.status));
            err.status = r.status;
            throw err;
          }
          return data;
        });
      });
    }
    if (method !== 'GET' && path.indexOf('/admin/') === 0 && !csrfToken) {
      var th = { 'Accept': 'application/json' };
      if (adminKey) th['x-admin-key'] = adminKey;
      return fetch('/admin/api/csrf-token', { headers: th, credentials: 'same-origin', cache: 'no-store' })
        .then(function (r) { return r.json(); })
        .then(function (d) { csrfToken = d.token || ''; if (!csrfToken) throw new Error('CSRF token unavailable'); headers['X-NexaRoute-CSRF'] = csrfToken; return doFetch(); });
    }
    if (method !== 'GET' && csrfToken) headers['X-NexaRoute-CSRF'] = csrfToken;
    return doFetch();
  }

  // ---- state ----
  var registry = [];      // node specs from the API
  var templates = [];
  var defs = [];
  var def = null;         // open definition (draft or inspected revision)
  var readOnly = false;
  var positions = {};     // node id -> {x, y} in logical canvas coords
  var selected = null;    // {kind: 'node'|'edge', id: string}
  var pendingSource = null; // node id awaiting a target click
  var dirty = false;
  var currentRunId = null;
  var zoom = 1, panX = 0, panY = 0; // canvas view only; never persisted

  function specFor(nodeId) {
    for (var i = 0; i < registry.length; i++) if (registry[i].id === nodeId) return registry[i];
    return null;
  }
  function fName(f) { return f.Name || f.name || ''; }
  function fType(f) { return f.Type || f.type || 'string'; }
  function fReq(f) { return !!(f.Required || f.required); }
  function fDesc(f) { return f.Description || f.description || ''; }
  function isTriggerNode(node) { var s = specFor(node.node_id); return s && s.category === 'trigger'; }
  function triggerDecl(nodeId) {
    if (!def) return null;
    for (var i = 0; i < (def.triggers || []).length; i++) if (def.triggers[i].name === nodeId) return def.triggers[i];
    return null;
  }
  function setDirty(v) {
    dirty = v;
    var b = $('#dirty-badge');
    if (b) b.hidden = !v;
  }
  function nodeTitle(node) { var s = specFor(node.node_id); return (s && s.title) || node.node_id; }

  // ---- layout: columns by depth from triggers, rows by order ----
  function layout() {
    if (!def) return;
    var depth = {};
    var nodes = def.nodes || [];
    nodes.forEach(function (n) { depth[n.id] = 0; });
    var changed = true, guard = 0;
    while (changed && guard++ < 100) {
      changed = false;
      (def.edges || []).forEach(function (e) {
        if (depth[e.to] <= depth[e.from]) { depth[e.to] = depth[e.from] + 1; changed = true; }
      });
    }
    var cols = {};
    nodes.forEach(function (n) {
      var d = depth[n.id] || 0;
      var row = cols[d] || 0;
      cols[d] = row + 1;
      if (!positions[n.id]) positions[n.id] = { x: 40 + d * 260, y: 40 + row * 150 };
    });
  }
  function isRTL() { return document.documentElement.dir === 'rtl'; }
  function applyZoomPan() {
    var vp = $('#canvas-viewport');
    if (vp) vp.style.transform = 'translate(' + panX + 'px,' + panY + 'px) scale(' + zoom + ')';
    var label = $('#zoom-label');
    if (label) label.textContent = Math.round(zoom * 100) + '%';
  }
  function setZoom(next, cx, cy) {
    next = Math.min(2, Math.max(0.25, next));
    if (cx !== undefined && cy !== undefined) {
      panX = cx - (cx - panX) * (next / zoom);
      panY = cy - (cy - panY) * (next / zoom);
    }
    zoom = next;
    applyZoomPan();
  }
  function px(x) { return isRTL() ? 1600 - x - 210 : x; } // mirror node x in RTL

  // ---- canvas rendering ----
  function renderCanvas() {
    var canvas = $('#canvas');
    var svg = $('#canvas-edges');
    if (!def) { canvas.innerHTML = '<p class="muted">Open a draft, or create one from a template.</p>'; svg.innerHTML = ''; return; }
    $('#canvas-title').textContent = 'Canvas' + (def.name ? ' — ' + def.name : '') + (readOnly ? ' (read-only revision)' : '');
    var badge = $('#scope-badge');
    badge.textContent = 'scope: ' + (def.tenant_scope ? def.tenant_scope : 'shared');
    var html = '';
    (def.nodes || []).forEach(function (n) {
      var p = positions[n.id] || { x: 40, y: 40 };
      var sel = selected && selected.kind === 'node' && selected.id === n.id ? ' selected' : '';
      var ro = readOnly ? ' readonly' : '';
      html += '<article class="node-card' + sel + ro + '" tabindex="0" role="button" aria-label="' + esc(nodeTitle(n)) + '" data-node="' + esc(n.id) + '" style="left:' + px(p.x) + 'px;top:' + p.y + 'px">'
        + '<b>' + esc(nodeTitle(n)) + '</b><small>' + esc(n.id) + ' · ' + esc(n.node_id) + '</small>'
        + '<div class="ports"><button type="button" data-connect="' + esc(n.id) + '">Connect from here</button>'
        + (readOnly ? '' : '<button type="button" data-del-node="' + esc(n.id) + '">Delete</button>') + '</div></article>';
    });
    canvas.innerHTML = html;
    // Edges as SVG lines between card centers. Each edge also gets a
    // wide transparent hit line so it can be clicked to select it.
    var lines = '';
    (def.edges || []).forEach(function (e) {
      var a = positions[e.from], b = positions[e.to];
      if (!a || !b) return;
      var x1 = px(a.x) + 105, y1 = a.y + 40, x2 = px(b.x) + 105, y2 = b.y + 40;
      var selE = selected && selected.kind === 'edge' && selected.id === e.id ? ' stroke="#71d7ca" stroke-width="3"' : ' stroke="#8fa1b8" stroke-width="1.5"';
      lines += '<line x1="' + x1 + '" y1="' + y1 + '" x2="' + x2 + '" y2="' + y2 + '" stroke="transparent" stroke-width="14" pointer-events="stroke" data-edge="' + esc(e.id) + '" />';
      lines += '<line x1="' + x1 + '" y1="' + y1 + '" x2="' + x2 + '" y2="' + y2 + '"' + selE + ' pointer-events="none" />';
      if (e.branch) {
        lines += '<text class="edge-label" x="' + ((x1 + x2) / 2) + '" y="' + ((y1 + y2) / 2 - 6) + '" text-anchor="middle" pointer-events="none">' + esc(e.branch) + '</text>';
      }
    });
    svg.innerHTML = lines;
    svg.onclick = function (ev) {
      var t = ev.target;
      if (t && t.dataset && t.dataset.edge) {
        selected = { kind: 'edge', id: t.dataset.edge };
        renderCanvas();
      }
    };
    bindCanvas();
    renderInspector();
  }
  function bindCanvas() {
    $$('#canvas .node-card').forEach(function (card) {
      var id = card.dataset.node;
      card.addEventListener('click', function () {
        if (pendingSource && pendingSource !== id) { completeConnection(id); return; }
        selected = { kind: 'node', id: id };
        renderCanvas();
      });
      card.addEventListener('keydown', function (e) {
        if (readOnly) return;
        if (e.target.tagName === 'BUTTON') return;
        var step = e.shiftKey ? 50 : 10;
        var p = positions[id];
        if (!p) return;
        if (e.key === 'ArrowLeft') { p.x -= step; e.preventDefault(); }
        else if (e.key === 'ArrowRight') { p.x += step; e.preventDefault(); }
        else if (e.key === 'ArrowUp') { p.y -= step; e.preventDefault(); }
        else if (e.key === 'ArrowDown') { p.y += step; e.preventDefault(); }
        else if (e.key === 'Delete' || e.key === 'Backspace') { deleteNode(id); e.preventDefault(); return; }
        else return;
        renderCanvas();
        // Re-render rebuilds the card: restore focus so repeated
        // arrow-key moves keep working.
        $$('#canvas .node-card').forEach(function (c) { if (c.dataset.node === id) c.focus(); });
      });
      if (!readOnly) {
        card.addEventListener('pointerdown', function (e) {
          if (e.target.tagName === 'BUTTON') return;
          var p = positions[id];
          var startX = e.clientX, startY = e.clientY, ox = p.x, oy = p.y;
          function move(ev) {
            var dx = (ev.clientX - startX) / zoom, dy = (ev.clientY - startY) / zoom;
            p.x = Math.max(0, ox + (isRTL() ? -dx : dx));
            p.y = Math.max(0, oy + dy);
            renderCanvas();
          }
          function up() {
            document.removeEventListener('pointermove', move);
            document.removeEventListener('pointerup', up);
          }
          document.addEventListener('pointermove', move);
          document.addEventListener('pointerup', up);
        });
      }
      $$('button', card).forEach(function (b) {
        b.addEventListener('pointerdown', function (e) { e.stopPropagation(); });
        b.addEventListener('click', function (e) {
          e.stopPropagation();
          if (b.dataset.connect) { startConnection(b.dataset.connect); }
          if (b.dataset.delNode) { deleteNode(b.dataset.delNode); }
        });
      });
    });
  }
  function startConnection(nodeId) {
    pendingSource = nodeId;
    selected = { kind: 'node', id: nodeId };
    renderInspector();
  }
  function completeConnection(targetId) {
    var from = pendingSource;
    pendingSource = null;
    if (!def || from === targetId) { renderCanvas(); return; }
    var src = null;
    (def.nodes || []).forEach(function (n) { if (n.id === from) src = n; });
    if (src && src.node_id === 'nexaroute.control.branch') {
      // Branch edge: ask for the branch in the inspector, then confirm.
      selected = { kind: 'node', id: from };
      renderCanvas();
      var body = $('#inspector-body');
      var branches = [];
      if (src.config) { if (src.config.true_branch) branches.push(src.config.true_branch); if (src.config.false_branch) branches.push(src.config.false_branch); }
      var opts = branches.map(function (b) { return '<option value="' + esc(b) + '">' + esc(b) + '</option>'; }).join('');
      body.innerHTML += '<div class="field-row"><label>Branch for the edge to ' + esc(targetId) + '</label>'
        + '<select id="branch-pick">' + opts + '</select>'
        + '<button class="button" id="branch-confirm" type="button">Add branch edge</button></div>';
      $('#branch-confirm').addEventListener('click', function () {
        addEdge(from, targetId, $('#branch-pick').value);
      });
      return;
    }
    addEdge(from, targetId, '');
  }
  function addEdge(from, to, branch) {
    if (!def) return;
    var dup = (def.edges || []).some(function (e) { return e.from === from && e.to === to && (e.branch || '') === (branch || ''); });
    if (dup) { showDiagnostics([{ msg: 'That edge already exists.' }]); return; }
    def.edges = def.edges || [];
    def.edges.push({ id: 'e' + (def.edges.length + 1) + '-' + Date.now().toString(36), from: from, to: to, branch: branch || undefined });
    setDirty(true);
    selected = { kind: 'edge', id: def.edges[def.edges.length - 1].id };
    renderCanvas();
  }
  function deleteNode(id) {
    if (!def || readOnly) return;
    def.nodes = (def.nodes || []).filter(function (n) { return n.id !== id; });
    def.edges = (def.edges || []).filter(function (e) { return e.from !== id && e.to !== id; });
    def.triggers = (def.triggers || []).filter(function (t) { return t.name !== id; });
    delete positions[id];
    if (selected && selected.id === id) selected = null;
    setDirty(true);
    renderCanvas();
  }
  function deleteEdge(id) {
    if (!def || readOnly) return;
    def.edges = (def.edges || []).filter(function (e) { return e.id !== id; });
    selected = null;
    setDirty(true);
    renderCanvas();
  }

  // ---- inspector: schema-driven forms from the registry spec ----
  function fieldInputHtml(prefix, f, value) {
    var name = fName(f), type = fType(f);
    var label = esc(name) + (fReq(f) ? ' *' : '') + (fDesc(f) ? ' — ' + esc(fDesc(f)) : '');
    if (type === 'boolean') {
      return '<div class="field-row"><label><input type="checkbox" data-field="' + esc(prefix + name) + '"' + (value ? ' checked' : '') + '> ' + label + '</label></div>';
    }
    if (type === 'number') {
      return '<div class="field-row"><label>' + label + '<input type="number" step="any" data-field="' + esc(prefix + name) + '" value="' + esc(value === undefined ? '' : value) + '"></label></div>';
    }
    if (type === 'array' || type === 'object') {
      var text = value === undefined ? '' : JSON.stringify(value);
      return '<div class="field-row"><label>' + label + ' (JSON)<textarea rows="3" data-field="' + esc(prefix + name) + '">' + esc(text) + '</textarea></label></div>';
    }
    var hint = type === 'secretRef' ? 'Reference only, never a value.' : '';
    return '<div class="field-row"><label>' + label + ' ' + hint + '<input type="text" data-field="' + esc(prefix + name) + '" value="' + esc(value === undefined ? '' : value) + '"></label></div>';
  }
  function bindFieldInputs(scope, onChange) {
    $$('[data-field]', scope).forEach(function (el) {
      el.addEventListener('change', function () {
        var key = el.dataset.field;
        var raw = el.type === 'checkbox' ? el.checked : el.value;
        onChange(key, raw, el);
      });
    });
  }
  function parseFieldValue(spec, key, raw, el) {
    var f = null;
    (spec || []).forEach(function (x) { if (fName(x) === key) f = x; });
    var type = f ? fType(f) : 'string';
    if (type === 'number') {
      if (raw === '' || raw === null) return undefined;
      var n = Number(raw);
      if (isNaN(n)) { el.style.borderColor = 'var(--bad)'; return undefined; }
      el.style.borderColor = '';
      return n;
    }
    if (type === 'boolean') return !!raw;
    if (type === 'array' || type === 'object') {
      if (raw === '' || raw === null) return undefined;
      try { var v = JSON.parse(raw); el.style.borderColor = ''; return v; }
      catch (e) { el.style.borderColor = 'var(--bad)'; return undefined; }
    }
    return raw === '' ? undefined : raw;
  }
  function renderInspector() {
    var body = $('#inspector-body');
    if (!def || !selected) { body.innerHTML = '<p class="muted">Select a node or an edge.</p>'; return; }
    if (selected.kind === 'edge') {
      var edge = null;
      (def.edges || []).forEach(function (e) { if (e.id === selected.id) edge = e; });
      if (!edge) { body.innerHTML = '<p class="muted">Edge not found.</p>'; return; }
      body.innerHTML = '<p><b>Edge</b><br>' + esc(edge.from) + ' → ' + esc(edge.to) + (edge.branch ? ' <span class="pill">branch ' + esc(edge.branch) + '</span>' : '') + '</p>'
        + (readOnly ? '' : '<button class="button secondary" id="edge-delete" type="button">Delete edge</button>');
      var d = $('#edge-delete');
      if (d) d.addEventListener('click', function () { deleteEdge(edge.id); });
      return;
    }
    var node = null;
    (def.nodes || []).forEach(function (n) { if (n.id === selected.id) node = n; });
    if (!node) { body.innerHTML = '<p class="muted">Node not found.</p>'; return; }
    var spec = specFor(node.node_id);
    if (!spec) { body.innerHTML = '<p class="muted">Unknown node ' + esc(node.node_id) + ' (registry has no such spec).</p>'; return; }
    var html = '<p><b>' + esc(spec.title || node.node_id) + '</b><br><small>' + esc(node.id) + ' · ' + esc(node.node_id) + '</small></p>'
      + '<p class="muted">' + esc(spec.description || '') + '</p>';
    if (pendingSource === node.id) html += '<p><span class="pill warn">Pick a target node on the canvas to connect from here.</span></p>';
    var trigger = isTriggerNode(node);
    if (trigger) {
      html += '<p class="muted">Trigger nodes take the run input verbatim and declare no input mapping. Their config lives on the trigger declaration.</p>';
    }
    var decl = trigger ? triggerDecl(node.id) : null;
    var cfgTarget = decl ? decl.config : node.config;
    html += '<h4>Config</h4>';
    (spec.config_schema || []).forEach(function (f) {
      html += fieldInputHtml('config.', f, cfgTarget ? cfgTarget[fName(f)] : undefined);
    });
    if (!(spec.config_schema || []).length) html += '<p class="muted">No config fields.</p>';
    if (!trigger) {
      html += '<h4>Input mapping</h4><p class="muted">Literals, or <code>$.path</code> references into the run input and earlier node outputs. When set, the mapping replaces the predecessor merge for this node.</p>';
      (spec.input_schema || []).forEach(function (f) {
        html += fieldInputHtml('input.', f, node.input ? node.input[fName(f)] : undefined);
      });
      if (!(spec.input_schema || []).length) html += '<p class="muted">No input fields.</p>';
    }
    html += (readOnly ? '' : '<button class="button secondary" id="node-connect" type="button">Connect from here</button> '
      + '<button class="button secondary" id="node-delete" type="button">Delete node</button>');
    body.innerHTML = html;
    bindFieldInputs(body, function (key, raw, el) {
      if (readOnly) return;
      var dot = key.indexOf('.');
      var section = key.slice(0, dot), name = key.slice(dot + 1);
      var schema = section === 'config' ? spec.config_schema : spec.input_schema;
      var value = parseFieldValue(schema, name, raw, el);
      if (section === 'config' && decl) {
        decl.config = decl.config || {};
        if (value === undefined) delete decl.config[name]; else decl.config[name] = value;
      } else if (section === 'config') {
        node.config = node.config || {};
        if (value === undefined) delete node.config[name]; else node.config[name] = value;
      } else {
        node.input = node.input || {};
        if (value === undefined) delete node.input[name]; else node.input[name] = value;
      }
      setDirty(true);
    });
    var c = $('#node-connect');
    if (c) c.addEventListener('click', function () { startConnection(node.id); });
    var d2 = $('#node-delete');
    if (d2) d2.addEventListener('click', function () { deleteNode(node.id); });
  }

  // ---- diagnostics ----
  function showDiagnostics(items) {
    var el = $('#diagnostics');
    if (!items || !items.length) { el.innerHTML = '<span class="ok">No diagnostics.</span>'; return; }
    el.innerHTML = items.map(function (it) {
      return '<div class="' + (it.ok ? 'ok' : 'err') + '">' + esc(it.msg) + '</div>';
    }).join('');
  }
  function clientDiagnostics() {
    var items = [];
    if (!def) return items;
    var nodes = def.nodes || [];
    var byId = {};
    nodes.forEach(function (n) { byId[n.id] = n; });
    var hasTrigger = nodes.some(isTriggerNode);
    if (!hasTrigger) items.push({ msg: 'No trigger node: a definition with no entry point can never run.' });
    var incoming = {};
    (def.edges || []).forEach(function (e) {
      if (!byId[e.from]) items.push({ msg: 'Edge ' + e.id + ' has unknown source ' + e.from + '.' });
      if (!byId[e.to]) items.push({ msg: 'Edge ' + e.id + ' has unknown target ' + e.to + '.' });
      incoming[e.to] = (incoming[e.to] || 0) + 1;
    });
    nodes.forEach(function (n) {
      if (!isTriggerNode(n) && !incoming[n.id]) items.push({ msg: 'Node ' + n.id + ' has no incoming edge.' });
      if (isTriggerNode(n) && incoming[n.id]) items.push({ msg: 'Trigger node ' + n.id + ' has an incoming edge.' });
    });
    // Cycle check (DFS).
    var adj = {};
    (def.edges || []).forEach(function (e) { (adj[e.from] = adj[e.from] || []).push(e.to); });
    var state = {};
    function visit(id) {
      if (state[id] === 2) return false;
      if (state[id] === 1) return true;
      state[id] = 1;
      var bad = false;
      (adj[id] || []).forEach(function (t) { if (visit(t)) bad = true; });
      state[id] = 2;
      return bad;
    }
    if (nodes.some(function (n) { return visit(n.id); })) items.push({ msg: 'Graph has a cycle.' });
    // Branch labels must match the branch node's configured names.
    (def.edges || []).forEach(function (e) {
      if (!e.branch) return;
      var src = byId[e.from];
      var names = [];
      if (src && src.config) { if (src.config.true_branch) names.push(src.config.true_branch); if (src.config.false_branch) names.push(src.config.false_branch); }
      if (names.length && names.indexOf(e.branch) === -1) items.push({ msg: 'Edge ' + e.id + ' branch ' + e.branch + ' is not one of ' + names.join(', ') + '.' });
    });
    if (!items.length) items.push({ ok: true, msg: 'Client checks pass. Server validation is still authoritative.' });
    return items;
  }

  // ---- palette ----
  function renderPalette() {
    var q = ($('#palette-search').value || '').toLowerCase();
    var byCat = {};
    registry.forEach(function (s) {
      if (q && (s.title + ' ' + s.id).toLowerCase().indexOf(q) === -1) return;
      (byCat[s.category || 'other'] = byCat[s.category || 'other'] || []).push(s);
    });
    var html = '';
    Object.keys(byCat).sort().forEach(function (cat) {
      html += '<h4>' + esc(cat) + '</h4>';
      byCat[cat].forEach(function (s) {
        html += '<button class="palette-item" type="button" data-add="' + esc(s.id) + '">' + esc(s.title || s.id) + '<small>' + esc(s.id) + '</small></button>';
      });
    });
    $('#palette-list').innerHTML = html || '<p class="muted">No nodes match.</p>';
    $$('#palette-list [data-add]').forEach(function (b) {
      b.addEventListener('click', function () { addNode(b.dataset.add); });
    });
  }
  function addNode(nodeId) {
    if (!def || readOnly) return;
    var base = nodeId.split('.').pop();
    var id = base, n = 2;
    var taken = {};
    (def.nodes || []).forEach(function (x) { taken[x.id] = true; });
    while (taken[id]) id = base + '-' + (n++);
    var spec = specFor(nodeId);
    def.nodes = def.nodes || [];
    def.nodes.push({ id: id, node_id: nodeId, version: spec ? spec.version : 'v1' });
    if (spec && spec.category === 'trigger') {
      var kind = nodeId.split('.').pop();
      def.triggers = def.triggers || [];
      def.triggers.push({ name: id, kind: kind });
    }
    positions[id] = { x: 40 + (def.nodes.length % 4) * 260, y: 40 + Math.floor(def.nodes.length / 4) * 150 };
    selected = { kind: 'node', id: id };
    setDirty(true);
    renderCanvas();
  }

  // ---- workflows + templates ----
  function renderWorkflows() {
    var q = ($('#wf-search').value || '').toLowerCase();
    var html = '';
    defs.forEach(function (d) {
      if (q && (d.id + ' ' + (d.name || '')).toLowerCase().indexOf(q) === -1) return;
      html += '<li><b>' + esc(d.id) + '</b> <small>' + esc(d.name || '') + ' · ' + (d.nodes || []).length + ' nodes' + (d.tenant_scope ? ' · scope ' + esc(d.tenant_scope) : '') + '</small>'
        + '<button class="button secondary" type="button" data-open="' + esc(d.id) + '">Open</button></li>';
    });
    $('#wf-list').innerHTML = html || '<li class="muted">No drafts yet.</li>';
    $$('#wf-list [data-open]').forEach(function (b) {
      b.addEventListener('click', function () { openDraft(b.dataset.open); });
    });
  }
  function renderTemplates() {
    var html = '';
    templates.forEach(function (t) {
      html += '<li><b>' + esc(t.title || t.id) + '</b> <small>' + esc(t.description || '') + (t.requires_policy ? ' · needs policy approval' : '') + '</small>'
        + '<button class="button secondary" type="button" data-tpl="' + esc(t.id) + '">Use template</button></li>';
    });
    $('#template-list').innerHTML = html || '<li class="muted">No templates.</li>';
    $$('#template-list [data-tpl]').forEach(function (b) {
      b.addEventListener('click', function () { useTemplate(b.dataset.tpl); });
    });
  }
  function useTemplate(id) {
    var tpl = null;
    templates.forEach(function (t) { if (t.id === id) tpl = t; });
    if (!tpl) return;
    var html = '<h3>Fill placeholders for ' + esc(tpl.title || tpl.id) + '</h3>';
    (tpl.placeholders || []).forEach(function (p) {
      html += '<div class="field-row"><label>' + esc(p) + '<input type="text" data-ph="' + esc(p) + '"></label></div>';
    });
    if (tpl.requires_policy) html += '<div class="field-row"><label><input type="checkbox" id="tpl-policy"> I approve the operator policy for this template (required).</label></div>';
    html += '<button class="button" id="tpl-create" type="button">Create draft</button>';
    $('#version-detail').innerHTML = html;
    $('#tpl-create').addEventListener('click', function () {
      var placeholders = {};
      $$('[data-ph]').forEach(function (el) { placeholders[el.dataset.ph] = el.value; });
      var approved = tpl.requires_policy ? !!$('#tpl-policy').checked : true;
      if (tpl.requires_policy && !approved) { showError('This template requires explicit policy approval.'); return; }
      api('/admin/api/nexaflow/templates/' + encodeURIComponent(id) + '/instantiate', { method: 'POST', body: { placeholders: placeholders, policy_approved: approved } })
        .then(function (r) { clearError(); $('#version-detail').innerHTML = ''; return openDraft(r.id); })
        .catch(function (e) { showError('Template instantiate failed: ' + e.message); });
    });
  }

  // ---- draft lifecycle ----
  function openDraft(id) {
    clearError();
    return api('/admin/api/nexaflow/definitions/' + encodeURIComponent(id))
      .then(function (d) {
        def = d; readOnly = false; positions = {}; selected = null; setDirty(false);
        layout(); renderCanvas(); renderVersions(); renderRuns();
      })
      .catch(function (e) { showError('Open draft failed: ' + e.message, function () { openDraft(id); }); });
  }
  function newDraft() {
    def = { schema_version: 'v1', id: '', name: '', nodes: [], edges: [], triggers: [], settings: {} };
    readOnly = false; positions = {}; selected = null; setDirty(true);
    showDiagnostics([{ msg: 'New draft: add a trigger node from the palette, then save with an id and name.' }]);
    renderCanvas();
  }
  function saveDraft() {
    if (!def) return Promise.resolve();
    if (readOnly) { showError('You are inspecting a published revision (read-only). Open the draft to edit.'); return Promise.resolve(); }
    if (!def.id) {
      var el = $('#diagnostics');
      el.innerHTML = '<div class="field-row"><label>Draft id <input id="new-id" type="text" placeholder="my-workflow"></label>'
        + '<label>Name <input id="new-name" type="text"></label>'
        + '<button class="button" id="new-confirm" type="button">Save with this id</button></div>';
      $('#new-confirm').addEventListener('click', function () {
        def.id = $('#new-id').value.trim();
        def.name = $('#new-name').value.trim();
        if (!def.id) { showError('A draft id is required.'); return; }
        saveDraft();
      });
      return Promise.resolve();
    }
    return api('/admin/api/nexaflow/definitions', { method: 'POST', body: def })
      .then(function () { clearError(); setDirty(false); showDiagnostics([{ ok: true, msg: 'Draft saved.' }]); return refreshDefs(); })
      .catch(function (e) { showError('Save failed: ' + e.message); });
  }
  function refreshDefs() {
    var q = $('#wf-search').value || '';
    return api('/admin/api/nexaflow/definitions' + (q ? '?q=' + encodeURIComponent(q) : ''))
      .then(function (r) { defs = r.definitions || []; renderWorkflows(); })
      .catch(function (e) { showError('Load drafts failed: ' + e.message, refreshDefs); });
  }
  function refreshRegistry() {
    return api('/admin/api/nexaflow/nodes')
      .then(function (r) { registry = r.nodes || []; renderPalette(); })
      .catch(function (e) { showError('Load registry failed: ' + e.message, refreshRegistry); });
  }
  function refreshTemplates() {
    return api('/admin/api/nexaflow/templates')
      .then(function (r) { templates = r.templates || []; renderTemplates(); })
      .catch(function (e) { showError('Load templates failed: ' + e.message, refreshTemplates); });
  }

  // ---- versions ----
  function renderVersions() {
    if (!def || !def.id) { $('#version-list').innerHTML = '<li class="muted">Save a draft first.</li>'; return; }
    api('/admin/api/nexaflow/definitions/' + encodeURIComponent(def.id) + '/versions')
      .then(function (r) {
        var revs = r.versions || [];
        var html = '';
        if (readOnly) html += '<li><button class="button" id="back-to-draft" type="button">Back to draft</button> <small>You are inspecting a revision read-only.</small></li>';
        revs.forEach(function (p) {
          var v = p.version || {};
          html += '<li>Revision ' + esc(v.revision) + ' · ' + esc(v.status || '') + (p.published_at ? ' · ' + esc(p.published_at) : '') + (p.published_by ? ' by ' + esc(p.published_by) : '')
            + '<button class="button secondary" type="button" data-inspect="' + esc(v.revision) + '">Inspect</button></li>';
        });
        $('#version-list').innerHTML = html || '<li class="muted">No published revisions.</li>';
        var back = $('#back-to-draft');
        if (back) back.addEventListener('click', function () { openDraft(def.id); });
        $$('#version-list [data-inspect]').forEach(function (b) {
          b.addEventListener('click', function () { inspectRevision(b.dataset.inspect); });
        });
      })
      .catch(function (e) { showError('Load versions failed: ' + e.message); });
  }
  function inspectRevision(rev) {
    if (!def) return;
    api('/admin/api/nexaflow/definitions/' + encodeURIComponent(def.id) + '/versions/' + encodeURIComponent(rev))
      .then(function (p) {
        def = p.definition; readOnly = true; positions = {}; selected = null; setDirty(false);
        layout(); renderCanvas();
        showDiagnostics([{ ok: true, msg: 'Inspecting revision ' + rev + ' read-only.' }]);
      })
      .catch(function (e) { showError('Inspect revision failed: ' + e.message); });
  }

  // ---- runs ----
  function renderRuns() {
    if (!def || !def.id) { $('#run-list').innerHTML = '<li class="muted">Save a draft first.</li>'; return; }
    api('/admin/api/nexaflow/runs?definition_id=' + encodeURIComponent(def.id))
      .then(function (r) {
        var html = '';
        (r.runs || []).forEach(function (run) {
          html += '<li><b>' + esc(run.id) + '</b> <span class="pill">' + esc(run.state) + '</span> <small>rev ' + esc(run.revision) + ' · ' + esc(run.principal || '') + '</small>'
            + '<button class="button secondary" type="button" data-run="' + esc(run.id) + '">Timeline</button></li>';
        });
        $('#run-list').innerHTML = html || '<li class="muted">No runs yet.</li>';
        $$('#run-list [data-run]').forEach(function (b) {
          b.addEventListener('click', function () { showRun(b.dataset.run); });
        });
      })
      .catch(function (e) { showError('Load runs failed: ' + e.message); });
  }
  function durationMs(a, b) {
    if (!a || !b) return '—';
    var ms = new Date(b).getTime() - new Date(a).getTime();
    return isNaN(ms) ? '—' : ms + ' ms';
  }
  function showRun(id) {
    currentRunId = id;
    api('/admin/api/nexaflow/runs/' + encodeURIComponent(id))
      .then(function (r) {
        var run = r.run || {};
        var html = '<h3>Run ' + esc(run.id) + ' <span class="pill">' + esc(run.state) + '</span></h3>'
          + '<p class="muted">Definition ' + esc(run.definition_id) + ' rev ' + esc(run.revision)
          + (run.error_code ? ' · error ' + esc(run.error_code) : '') + '</p>';
        if (run.state !== 'succeeded' && run.state !== 'failed' && run.state !== 'cancelled') {
          html += '<button class="button secondary" id="run-cancel" type="button">Cancel run</button> ';
        }
        html += '<div class="timeline">';
        (r.nodes || []).forEach(function (n) {
          var pill = n.state === 'succeeded' ? 'good' : (n.state === 'failed' || n.state === 'needs_manual_action' ? 'bad' : 'warn');
          html += '<div class="timeline-row"><div><b>' + esc(n.node_id) + '</b> <span class="pill ' + pill + '">' + esc(n.state) + '</span>'
            + '<small> attempt ' + esc(n.attempt) + ' · ' + durationMs(n.started_at, n.finished_at)
            + (n.output_truncated ? ' · output truncated' : '') + (n.ambiguous ? ' · ambiguous' : '')
            + (n.error_code ? ' · error ' + esc(n.error_code) : '') + '</small>'
            + '<pre>' + esc(JSON.stringify(n.output || {}, null, 2)) + '</pre></div><div>';
          if (n.state === 'waiting') {
            html += '<button class="button secondary" type="button" data-approve="' + esc(n.node_id) + '" data-decision="approved">Approve</button> '
              + '<button class="button secondary" type="button" data-approve="' + esc(n.node_id) + '" data-decision="rejected">Reject</button>';
          }
          html += '</div></div>';
        });
        html += '</div>';
        $('#run-detail').innerHTML = html;
        var c = $('#run-cancel');
        if (c) c.addEventListener('click', function () {
          api('/admin/api/nexaflow/runs/' + encodeURIComponent(id) + '/cancel', { method: 'POST', body: {} })
            .then(function () { showRun(id); renderRuns(); })
            .catch(function (e) { showError('Cancel failed: ' + e.message); });
        });
        $$('[data-approve]').forEach(function (b) {
          b.addEventListener('click', function () {
            api('/admin/api/nexaflow/runs/' + encodeURIComponent(id) + '/approve', { method: 'POST', body: { node_id: b.dataset.approve, decision: b.dataset.decision } })
              .then(function () { showRun(id); renderRuns(); })
              .catch(function (e) { showError('Approval failed: ' + e.message); });
          });
        });
      })
      .catch(function (e) { showError('Load run failed: ' + e.message); });
  }
  function startRun() {
    if (!def || !def.id) { showError('Open a draft first.'); return; }
    if (readOnly) { showError('You are inspecting a published revision. Open the draft to start a run from its latest revision.'); return; }
    var input = {};
    var raw = $('#run-input').value.trim();
    if (raw) {
      try { input = JSON.parse(raw); }
      catch (e) { showError('Run input is not valid JSON.'); return; }
    }
    var key = (window.crypto && crypto.randomUUID) ? crypto.randomUUID() : ('run-' + Date.now().toString(36));
    api('/admin/api/nexaflow/definitions/' + encodeURIComponent(def.id) + '/runs', { method: 'POST', body: { input: input, idempotency_key: key } })
      .then(function (r) { clearError(); renderRuns(); showRun(r.id); })
      .catch(function (e) { showError('Start run failed: ' + e.message); });
  }

  // ---- audit ----
  function refreshAudit() {
    api('/admin/api/nexaflow/audit')
      .then(function (r) {
        var html = '';
        (r.audit || []).slice(-50).forEach(function (a) {
          html += '<li><b>' + esc(a.kind) + '</b> <small>' + esc(a.at || '') + (a.actor ? ' · ' + esc(a.actor) : '') + (a.run_id ? ' · run ' + esc(a.run_id) : '') + (a.detail ? ' · ' + esc(a.detail) : '') + '</small></li>';
        });
        $('#audit-list').innerHTML = html || '<li class="muted">No audit records.</li>';
      })
      .catch(function (e) { showError('Load audit failed: ' + e.message); });
  }

  // ---- toolbar wiring ----
  function wire() {
    $('#connect-btn').addEventListener('click', function () {
      adminKey = $('#admin-key').value.trim();
      if (!adminKey) { showError('Enter the admin key first.'); return; }
      sessionStorage.setItem('nexaflow_studio_key', adminKey);
      csrfToken = '';
      $('#auth-status').textContent = 'Connecting…';
      Promise.all([refreshRegistry(), refreshTemplates(), refreshDefs()])
        .then(function () {
          clearError();
          $('#auth-status').textContent = 'Connected.';
          $('#studio').hidden = false;
          if (!localStorage.getItem('nexaflow_studio_onboarded')) $('#onboarding').hidden = false;
        })
        .catch(function () { $('#auth-status').textContent = 'Connection failed.'; });
    });
    $('#onboarding-dismiss').addEventListener('click', function () {
      localStorage.setItem('nexaflow_studio_onboarded', '1');
      $('#onboarding').hidden = true;
    });
    $('#dir-select').addEventListener('change', function (e) {
      document.documentElement.dir = e.target.value;
      localStorage.setItem('nexaflow_studio_dir', e.target.value);
      renderCanvas();
    });
    $('#theme-select').addEventListener('change', function (e) {
      document.documentElement.dataset.theme = e.target.value;
      localStorage.setItem('nexaflow_studio_theme', e.target.value);
    });
    $('#wf-search').addEventListener('input', renderWorkflows);
    $('#wf-refresh').addEventListener('click', refreshDefs);
    $('#wf-new').addEventListener('click', newDraft);
    $('#palette-search').addEventListener('input', renderPalette);
    $('#btn-validate').addEventListener('click', function () {
      if (!def) return;
      showDiagnostics(clientDiagnostics());
      api('/admin/api/nexaflow/definitions/validate', { method: 'POST', body: { definition: def } })
        .then(function (r) {
          var items = clientDiagnostics();
          if (r.valid) items.push({ ok: true, msg: 'Server validation: valid.' });
          else (r.errors || []).forEach(function (e) { items.push({ msg: 'Server: ' + e }); });
          if ((r.secret_refs || []).length) items.push({ ok: true, msg: 'Secret refs: ' + r.secret_refs.join(', ') + ' (references only).' });
          showDiagnostics(items);
        })
        .catch(function (e) { showError('Validate failed: ' + e.message); });
    });
    $('#btn-save').addEventListener('click', saveDraft);
    $('#btn-publish').addEventListener('click', function () {
      if (!def || !def.id) { showError('Save a draft first.'); return; }
      if (readOnly) { showError('You are inspecting a published revision (read-only). Open the draft to publish.'); return; }
      var p = dirty ? saveDraft().then(function () {
        return api('/admin/api/nexaflow/definitions/' + encodeURIComponent(def.id) + '/publish', { method: 'POST', body: {} });
      }) : api('/admin/api/nexaflow/definitions/' + encodeURIComponent(def.id) + '/publish', { method: 'POST', body: {} });
      p.then(function (r) {
        clearError();
        showDiagnostics([{ ok: true, msg: 'Published as revision ' + r.revision + '.' }]);
        renderVersions();
      }).catch(function (e) { showError('Publish failed: ' + e.message); });
    });
    $('#btn-versions').addEventListener('click', renderVersions);
    $('#btn-zoom-in').addEventListener('click', function () { setZoom(zoom * 1.25); });
    $('#btn-zoom-out').addEventListener('click', function () { setZoom(zoom / 1.25); });
    $('#btn-zoom-reset').addEventListener('click', function () { zoom = 1; panX = 0; panY = 0; applyZoomPan(); });
    (function () {
      var wrap = $('#canvas-wrap');
      if (!wrap) return;
      wrap.addEventListener('wheel', function (e) {
        if (!e.ctrlKey) return;
        e.preventDefault();
        var rect = wrap.getBoundingClientRect();
        setZoom(zoom * (e.deltaY < 0 ? 1.25 : 0.8), e.clientX - rect.left, e.clientY - rect.top);
      }, { passive: false });
      wrap.addEventListener('pointerdown', function (e) {
        var t = e.target;
        if (t.closest && t.closest('.node-card')) return;
        if (t.tagName === 'LINE' || t.tagName === 'TEXT') return;
        if (t !== wrap && t.id !== 'canvas-viewport' && t.id !== 'canvas') return;
        var startX = e.clientX, startY = e.clientY, ox = panX, oy = panY;
        function move(ev) {
          panX = ox + (ev.clientX - startX);
          panY = oy + (ev.clientY - startY);
          applyZoomPan();
        }
        function up() {
          document.removeEventListener('pointermove', move);
          document.removeEventListener('pointerup', up);
        }
        document.addEventListener('pointermove', move);
        document.addEventListener('pointerup', up);
      });
    })();
    $('#btn-run').addEventListener('click', startRun);
    $('#runs-refresh').addEventListener('click', renderRuns);
    $('#audit-refresh').addEventListener('click', refreshAudit);
    document.addEventListener('keydown', function (e) {
      if ((e.key === 'Delete' || e.key === 'Backspace') && selected && selected.kind === 'edge' && !readOnly) {
        var tag = (e.target.tagName || '').toLowerCase();
        if (tag === 'input' || tag === 'textarea' || tag === 'select') return;
        deleteEdge(selected.id);
      }
    });
  }

  // ---- init ----
  (function init() {
    var dir = localStorage.getItem('nexaflow_studio_dir') || 'ltr';
    document.documentElement.dir = dir;
    $('#dir-select').value = dir;
    var theme = localStorage.getItem('nexaflow_studio_theme') || 'dark';
    document.documentElement.dataset.theme = theme;
    $('#theme-select').value = theme;
    if (adminKey) $('#admin-key').value = adminKey;
    wire();
    if (adminKey) $('#connect-btn').click();
  })();
})();
