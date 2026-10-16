// brojev in-page library. Runs in an isolated world per frame, so page
// scripts cannot see or tamper with it. Element refs are stable integer ids
// held through WeakRefs: a ref stays valid while its node stays connected.
(() => {
  const VERSION = 7;
  if (globalThis.__bj && globalThis.__bj.v === VERSION) return;

  const ids = new WeakMap();
  const refs = new Map();
  let next = 1;

  const idOf = (el) => {
    let id = ids.get(el);
    if (!id) {
      id = next++;
      ids.set(el, id);
      refs.set(id, new WeakRef(el));
    }
    return id;
  };

  const get = (id) => {
    const el = refs.get(id)?.deref();
    if (!el || !el.isConnected) throw new Error("stale ref: element is gone, take a new snapshot");
    return el;
  };

  const prune = () => {
    for (const [id, r] of refs) {
      const el = r.deref();
      if (!el || !el.isConnected) refs.delete(id);
    }
  };

  const clean = (s, max = 80) => {
    s = (s || "").replace(/\s+/g, " ").trim();
    return s.length > max ? s.slice(0, max - 1) + "…" : s;
  };

  const TEXT_INPUTS = new Set(["", "text", "search", "email", "tel", "url", "password", "number"]);
  const INTERACTIVE_ROLES = new Set([
    "button", "link", "checkbox", "radio", "switch", "tab", "menuitem", "menuitemcheckbox",
    "menuitemradio", "option", "combobox", "textbox", "searchbox", "slider", "spinbutton",
    "listbox", "treeitem", "gridcell",
  ]);

  const role = (el) => {
    const explicit = el.getAttribute("role");
    if (explicit) return explicit.split(" ")[0];
    const tag = el.localName;
    switch (tag) {
      case "a": return el.hasAttribute("href") ? "link" : "";
      case "button": case "summary": return "button";
      case "select": return el.multiple || el.size > 1 ? "listbox" : "combobox";
      case "textarea": return "textbox";
      case "option": return "option";
      case "input": {
        const t = (el.getAttribute("type") || "").toLowerCase();
        if (["button", "submit", "reset", "image"].includes(t)) return "button";
        if (t === "checkbox") return "checkbox";
        if (t === "radio") return "radio";
        if (t === "range") return "slider";
        if (t === "number") return "spinbutton";
        if (t === "file") return "file";
        if (t === "search") return "searchbox";
        if (el.hasAttribute("list")) return "combobox";
        if (TEXT_INPUTS.has(t)) return "textbox";
        return t; // date, time, color, ...: keep the type as the role
      }
    }
    if (el.isContentEditable && (el.parentElement === null || !el.parentElement.isContentEditable)) return "textbox";
    if (el.getAttribute("draggable") === "true") return "draggable";
    return "";
  };

  const INLINE = new Set(["span", "a", "b", "i", "em", "strong", "small", "abbr", "code", "label", "sup", "sub", "mark", "s", "u", "time", "bdi", "kbd", "q", "cite", "font"]);

  // nameFromContent follows accname: a descendant's aria-label (or img alt)
  // replaces its text, so a day cell "20" whose child is labelled
  // "Friday, November 20, 2026" gets the full date.
  const nameFromContent = (el, budget = { n: 400 }) => {
    let s = "";
    for (const c of el.childNodes) {
      if (budget.n <= 0) break;
      if (c.nodeType === 3) {
        s += c.nodeValue;
        budget.n -= c.nodeValue.length;
        continue;
      }
      if (c.nodeType !== 1) continue;
      if (c.getAttribute("aria-hidden") === "true" || ["script", "style", "template"].includes(c.localName)) continue;
      const label = c.getAttribute("aria-label");
      if (label && label.trim()) {
        s += " " + label + " ";
        budget.n -= label.length;
      } else if (c.localName === "img" && c.alt) {
        s += " " + c.alt + " ";
      } else if (c.checkVisibility && !c.checkVisibility({ visibilityProperty: true })) {
        continue;
      } else {
        const block = !INLINE.has(c.localName); // tag check: getComputedStyle per child is too slow
        s += (block ? " " : "") + nameFromContent(c, budget) + (block ? " " : "");
      }
    }
    if (el.shadowRoot && !s.trim()) s = nameFromContent(el.shadowRoot, budget);
    return s;
  };

  const textOf = (node) => {
    // innerText respects CSS visibility but not shadow trees; fall back to textContent.
    return node.innerText !== undefined ? node.innerText : node.textContent;
  };

  const name = (el) => {
    const by = el.getAttribute("aria-labelledby");
    if (by) {
      const root = el.getRootNode();
      const s = by.split(/\s+/).map((id) => root.getElementById?.(id) || document.getElementById(id))
        .filter(Boolean).map(textOf).join(" ");
      if (clean(s)) return clean(s);
    }
    const aria = el.getAttribute("aria-label");
    if (aria && aria.trim()) return clean(aria);
    if (el.labels && el.labels.length) {
      const s = Array.from(el.labels).map(textOf).join(" ");
      if (clean(s)) return clean(s);
    }
    const tag = el.localName;
    if (tag === "input") {
      const t = (el.type || "").toLowerCase();
      if (["button", "submit", "reset"].includes(t) && el.value) return clean(el.value);
      if (t === "image" && el.alt) return clean(el.alt);
      if (el.placeholder) return clean(el.placeholder);
    }
    if (tag === "textarea" && el.placeholder) return clean(el.placeholder);
    if (tag === "img" && el.alt) return clean(el.alt);
    if (tag !== "select" && tag !== "textarea" && tag !== "input" && !el.isContentEditable) {
      const s = clean(nameFromContent(el));
      if (s) return s;
      const img = el.querySelector("img[alt], svg title, [aria-label]");
      if (img) return clean(img.getAttribute("alt") || img.getAttribute("aria-label") || img.textContent);
    }
    if (el.title) return clean(el.title);
    if (toggle(el)) {
      // Unlabelled toggle (a todo-list row): use the neighbouring label or the row text.
      const sib = el.nextElementSibling || el.previousElementSibling;
      if (sib && sib.localName === "label") {
        const s = clean(textOf(sib));
        if (s) return s;
      }
      const row = el.closest("li,tr,[role=row],[role=listitem],[role=option]");
      if (row) return clean(textOf(row));
    }
    return "";
  };

  const value = (el) => {
    const tag = el.localName;
    if (tag === "select") {
      return clean(Array.from(el.selectedOptions).map((o) => o.label || o.text).join(", "));
    }
    if (tag === "input" || tag === "textarea") {
      const t = (el.type || "").toLowerCase();
      if (["checkbox", "radio", "button", "submit", "reset", "image", "file"].includes(t)) {
        if (t === "file") return clean(Array.from(el.files || []).map((f) => f.name).join(", "));
        return undefined;
      }
      if (t === "password") return el.value ? "•".repeat(Math.min(el.value.length, 8)) : "";
      return clean(el.value, 120);
    }
    if (el.isContentEditable) return clean(el.innerText, 120);
    const v = el.getAttribute("aria-valuetext") || el.getAttribute("aria-valuenow");
    return v == null ? undefined : clean(v);
  };

  const states = (el) => {
    const s = [];
    const aria = (a) => el.getAttribute(a);
    if (el.disabled || el.closest?.("[aria-disabled=true]") || el.closest?.("fieldset[disabled]")) s.push("disabled");
    if (el.checked || aria("aria-checked") === "true") s.push("checked");
    if (aria("aria-checked") === "mixed" || el.indeterminate) s.push("mixed");
    if (aria("aria-selected") === "true" || (el.localName === "option" && el.selected)) s.push("selected");
    if (aria("aria-pressed") === "true") s.push("pressed");
    if (aria("aria-expanded") === "true") s.push("expanded");
    if (aria("aria-expanded") === "false") s.push("collapsed");
    if (el.readOnly || aria("aria-readonly") === "true") s.push("readonly");
    if (el.required || aria("aria-required") === "true") s.push("required");
    let a = document.activeElement;
    while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement;
    if (a === el) s.push("focused");
    return s;
  };

  // Custom checkboxes/radios often hide the real input (opacity 0 or zero
  // size) and draw the control with its label. They still receive clicks at
  // the input's box or through the label, so they count as visible.
  const toggle = (el) => el.localName === "input" && ((el.type || "").toLowerCase() === "checkbox" || (el.type || "").toLowerCase() === "radio");
  const visible = (el) => {
    const r = el.getBoundingClientRect();
    if (toggle(el)) {
      if (el.checkVisibility && !el.checkVisibility({ visibilityProperty: true })) return null;
      if (r.width > 0 && r.height > 0) return r;
      const l = el.labels && el.labels[0];
      if (l) {
        const lr = l.getBoundingClientRect();
        if (lr.width > 0 && lr.height > 0 && (!l.checkVisibility || l.checkVisibility({ opacityProperty: true, visibilityProperty: true }))) return lr;
      }
      return null;
    }
    if (r.width <= 0 || r.height <= 0) return null;
    if (el.checkVisibility && !el.checkVisibility({ opacityProperty: true, visibilityProperty: true })) return null;
    return r;
  };

  // pointerOwner: element is styled clickable while its parent is not, which
  // catches div/span "buttons" with JS listeners we cannot see from here.
  const pointerOwner = (el) => {
    if (getComputedStyle(el).cursor !== "pointer") return false;
    const p = el.parentElement || el.getRootNode().host;
    return !p || getComputedStyle(p).cursor !== "pointer";
  };

  const interactive = (el) => {
    const tag = el.localName;
    if (tag === "input") return (el.type || "").toLowerCase() !== "hidden";
    if (["button", "select", "textarea", "summary"].includes(tag)) return true;
    if (tag === "a" && el.hasAttribute("href")) return true;
    const r = el.getAttribute("role");
    if (r && INTERACTIVE_ROLES.has(r.split(" ")[0])) return true;
    if (el.isContentEditable && !(el.parentElement && el.parentElement.isContentEditable)) return true;
    if (el.hasAttribute("onclick")) return true;
    if (el.getAttribute("draggable") === "true") return true;
    const ti = el.getAttribute("tabindex");
    if (ti !== null && Number(ti) >= 0 && tag !== "body" && tag !== "html") return true;
    return false;
  };

  const CONTROL = "a[href],button,input:not([type=hidden]),select,textarea,summary,[role=button],[role=link],[role=checkbox],[role=tab],[role=menuitem],[role=option],[contenteditable=true]";

  const CONTAINER_ROLES = new Set(["gridcell", "row", "listitem", "treeitem", "cell"]);

  // Native widgets whose descendants are not separately actionable.
  const LEAF = new Set(["select", "button", "textarea", "input", "a", "summary"]);

  const snapshot = (opts) => {
    const max = opts.maxElements || 600;
    const maxOff = opts.maxOffscreen ?? 150;
    let offCount = 0;
    const maxText = opts.maxText ?? 6000;
    const vw = innerWidth, vh = innerHeight;
    const out = [];
    const frames = [];
    let images = 0;

    const consider = (el, insideInteractive) => {
      let isI = interactive(el);
      // Grid/list containers that wrap a real control: list the control.
      if (isI && CONTAINER_ROLES.has(el.getAttribute("role")) && el.querySelector(CONTROL)) isI = false;
      // A styled-clickable wrapper around a real control is noise: the
      // control itself is listed instead.
      const isPointer = !isI && !insideInteractive && (offCount <= maxOff) && pointerOwner(el) && !el.querySelector(CONTROL);
      const isImage = !isI && !isPointer && !insideInteractive && (el.localName === "img" || el.localName === "svg") && images < 40;
      if (!isI && !isPointer && !isImage) return false;
      const r = visible(el);
      if (!r) return isI;
      if (out.length >= max) return isI;
      const inView = r.bottom > 0 && r.right > 0 && r.top < vh && r.left < vw;
      if (!inView && ++offCount > maxOff) return isI; // skip naming work for the long tail
      if (isImage) {
        const alt = el.getAttribute("alt") || el.getAttribute("aria-label") || el.querySelector?.("title")?.textContent || "";
        if (!alt.trim() && (r.width < 48 || r.height < 48)) return false; // icons and spacers
        images++;
      }
      const item = {
        id: idOf(el),
        role: isImage ? "image" : role(el) || (isPointer ? "clickable" : el.localName),
        name: name(el),
        tag: el.localName,
        rect: [Math.round(r.x), Math.round(r.y), Math.round(r.width), Math.round(r.height)],
        inView,
      };
      const v = value(el);
      if (v !== undefined) item.value = v;
      const st = states(el);
      if (st.length) item.states = st;
      if (el.localName === "a" && el.href) item.href = el.href.slice(0, 200);
      if (el.localName === "select") {
        item.options = Array.from(el.options).slice(0, 50).map((o) => clean(o.label || o.text, 60));
      }
      if (el.localName === "input" && el.type) item.type = el.type;
      out.push(item);
      return true;
    };

    const visit = (el, insideInteractive) => {
      if (el.localName === "iframe" || el.localName === "frame") {
        const r = visible(el);
        if (r) frames.push({ rect: [r.x, r.y, r.width, r.height] });
        return;
      }
      if (el.localName === "script" || el.localName === "style" || el.localName === "template") return;
      const taken = consider(el, insideInteractive);
      const inside = insideInteractive || taken;
      if (taken && LEAF.has(el.localName)) return;
      if (el.shadowRoot) for (const c of el.shadowRoot.children) visit(c, inside);
      for (const c of el.children) visit(c, inside);
    };

    if (document.documentElement) visit(document.documentElement, false);
    prune();

    // Visible text, for reading the page. Parent rects are cached per element.
    let text = "";
    if (maxText > 0 && document.body) {
      const parts = [];
      let len = 0;
      const seen = new Map();
      const inView = (p) => {
        if (seen.has(p)) return seen.get(p);
        const r = visible(p);
        const ok = !!r && r.bottom > 0 && r.top < vh && r.right > 0 && r.left < vw;
        seen.set(p, ok);
        return ok;
      };
      const walkText = (root) => {
        const tw = document.createTreeWalker(root, NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_TEXT);
        for (let n = tw.nextNode(); n && len < maxText; n = tw.nextNode()) {
          if (n.nodeType === 1) {
            if (n.shadowRoot) walkText(n.shadowRoot);
            continue;
          }
          const p = n.parentElement;
          if (!p || ["SCRIPT", "STYLE", "NOSCRIPT", "TEMPLATE"].includes(p.tagName)) continue;
          const s = n.nodeValue.replace(/\s+/g, " ").trim();
          if (!s || !inView(p)) continue;
          parts.push(s);
          len += s.length + 1;
        }
      };
      walkText(document.body);
      text = parts.join(" ").slice(0, maxText);
    }

    const se = document.scrollingElement || document.documentElement;
    return {
      url: location.href,
      title: document.title,
      viewport: [vw, vh],
      scroll: [Math.round(se.scrollLeft), Math.round(se.scrollTop), se.scrollWidth, se.scrollHeight],
      elements: out,
      frames,
      text,
    };
  };

  const frame = () => new Promise((resolve) => {
    let done = false;
    requestAnimationFrame(() => { done = true; resolve(); });
    setTimeout(() => { if (!done) resolve(); }, 50); // rAF stalls in hidden tabs
  });

  const deepHit = (x, y) => {
    let e = document.elementFromPoint(x, y);
    while (e && e.shadowRoot) {
      const inner = e.shadowRoot.elementFromPoint(x, y);
      if (!inner || inner === e) break;
      e = inner;
    }
    return e;
  };

  const owns = (el, hit) => {
    for (let n = hit; n; n = n.parentNode || n.host) if (n === el) return true;
    if (el.labels) for (const l of el.labels) if (l.contains(hit)) return true;
    return false;
  };

  const describe = (el) => {
    if (!el) return "nothing";
    const n = name(el);
    return `<${el.localName}${el.id ? "#" + el.id : ""}>${n ? ' "' + n + '"' : ""}`;
  };

  // prep makes an element ready for pointer input and returns a point inside
  // it that actually receives events, in this frame's viewport coordinates.
  const prep = async (id, opts) => {
    const el = get(id);
    const need = opts || {};
    let r = visible(el) || el.getBoundingClientRect();
    if (r.top < 0 || r.left < 0 || r.bottom > innerHeight || r.right > innerWidth) {
      el.scrollIntoView({ block: "center", inline: "center", behavior: "instant" });
    }
    // Stable: same box across a frame, so an animating target is not missed.
    for (let i = 0; i < 3; i++) {
      r = visible(el) || el.getBoundingClientRect();
      await frame();
      const r2 = visible(el) || el.getBoundingClientRect();
      if (r.x === r2.x && r.y === r2.y && r.width === r2.width && r.height === r2.height) break;
      r = r2;
    }
    if (!visible(el)) return { ok: false, reason: "not visible" };
    const st = states(el);
    if (need.enabled !== false && st.includes("disabled")) return { ok: false, reason: "disabled" };
    if (need.editable) {
      const editable = el.isContentEditable || ((el.localName === "input" || el.localName === "textarea") && !el.readOnly);
      if (!editable) return { ok: false, reason: "not editable" };
    }
    if (need.focus) el.focus({ preventScroll: true });
    const points = [[0.5, 0.5], [0.25, 0.5], [0.75, 0.5], [0.5, 0.25], [0.5, 0.75]];
    let blocker = null;
    for (const [fx, fy] of points) {
      const x = r.left + r.width * fx, y = r.top + r.height * fy;
      if (x < 0 || y < 0 || x >= innerWidth || y >= innerHeight) continue;
      const hit = deepHit(x, y);
      if (owns(el, hit)) return { ok: true, x, y, rect: [r.x, r.y, r.width, r.height], tag: el.localName, type: (el.type || "").toLowerCase() };
      blocker = blocker || hit;
    }
    return { ok: false, reason: "covered by " + describe(blocker) };
  };

  const focusEl = (id, clear) => {
    const el = get(id);
    el.focus({ preventScroll: true });
    if (clear) {
      if (typeof el.select === "function" && (el.localName === "input" || el.localName === "textarea")) {
        el.select();
      } else if (el.isContentEditable) {
        const range = document.createRange();
        range.selectNodeContents(el);
        const sel = getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
      }
    }
    let a = document.activeElement;
    while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement;
    return a === el || el.contains(a);
  };

  const selectOptions = (id, wanted) => {
    const el = get(id);
    if (el.localName !== "select") throw new Error("not a <select>: " + describe(el));
    const opts = Array.from(el.options);
    const pick = (w) => opts.find((o) => o.value === w) || opts.find((o) => (o.label || o.text).trim() === w)
      || opts.find((o) => (o.label || o.text).trim().toLowerCase() === w.trim().toLowerCase());
    const chosen = wanted.map((w) => {
      const o = pick(w);
      if (!o) throw new Error(`no option "${w}"; options: ${opts.map((o) => o.label || o.text).join(" | ")}`);
      return o;
    });
    if (!el.multiple && chosen.length > 1) throw new Error("single <select> takes one value");
    for (const o of opts) o.selected = chosen.includes(o);
    el.dispatchEvent(new Event("input", { bubbles: true, composed: true }));
    el.dispatchEvent(new Event("change", { bubbles: true }));
    return chosen.map((o) => o.label || o.text);
  };

  // Native pickers (date, time, color, range) are browser UI that input
  // events cannot drive, so their value is set directly.
  const setValue = (id, v) => {
    const el = get(id);
    const proto = el.localName === "textarea" ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, "value").set.call(el, v);
    el.dispatchEvent(new Event("input", { bubbles: true, composed: true }));
    el.dispatchEvent(new Event("change", { bubbles: true }));
    return el.value;
  };

  const frameBox = (el) => {
    const r = el.getBoundingClientRect();
    const cs = getComputedStyle(el);
    const left = r.left + el.clientLeft + parseFloat(cs.paddingLeft);
    const top = r.top + el.clientTop + parseFloat(cs.paddingTop);
    return { x: left, y: top, w: el.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight),
      h: el.clientHeight - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom) };
  };

  const hasText = (s) => {
    const t = document.body ? document.body.innerText : "";
    return t.includes(s);
  };

  globalThis.__bj = { v: VERSION, idOf, get, snapshot, prep, focusEl, selectOptions, setValue, frameBox, hasText, describe };
})();
