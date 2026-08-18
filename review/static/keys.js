// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Keyboard shortcuts, dispatched from the table the server sends in
// #keymap. That table also renders the help dialog, so the two cannot
// disagree about what a key does.

"use strict";

(function () {
	const page = document.body.dataset.page;
	const sections = JSON.parse(document.getElementById("keymap").textContent);
	const info = document.getElementById("viewinfo");
	const view = info ? info.dataset : {};

	// combos maps a prefix key ("g", "v") to the bindings that start with it.
	// Gerrit gives a combo about a second to be completed.
	const comboTimeout = 1000;
	let pending = null;
	let pendingTimer = null;

	// Bindings for this page, as a map from key spec to action name.
	const direct = new Map();
	const combo = new Map();
	const repeatable = new Set();
	for (const section of sections) {
		if (!section.pages.includes(page)) continue;
		for (const b of section.bindings) {
			for (const spec of b.keys) {
				const parts = spec.split(" ");
				if (parts.length === 2) {
					if (!combo.has(parts[0])) combo.set(parts[0], new Map());
					combo.get(parts[0]).set(parts[1], b.action);
				} else {
					direct.set(spec, b.action);
				}
				if (b.repeat) repeatable.add(spec);
			}
		}
	}

	// spec builds the key spec for an event, matching the notation the
	// server uses: "j", "J", "Mod+Enter", "Shift+ArrowLeft".
	function spec(e) {
		let s = "";
		if (e.ctrlKey || e.metaKey) s += "Mod+";
		let key = e.key;
		// A letter typed with shift held is the shifted letter, whatever
		// the browser reports. Without this, holding shift across the page
		// load that M causes and typing it again arrives as a plain m,
		// which is a different command.
		if (e.shiftKey && key.length === 1 && key >= "a" && key <= "z") {
			key = key.toUpperCase();
		}
		// Shift is part of the key itself for printable characters
		// ("J" not "Shift+j"), but must be named for the arrow keys.
		if (e.shiftKey && key.length > 1) s += "Shift+";
		return s + key;
	}

	function editing(el) {
		if (!el) return false;
		const tag = el.tagName;
		return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || el.isContentEditable;
	}

	document.addEventListener("keydown", function (e) {
		if (e.altKey) return;
		const s = spec(e);

		if (e.key === "Escape") {
			clearPending();
			const dlg = document.querySelector("dialog[open]");
			if (dlg) { dlg.close(); e.preventDefault(); return; }
			if (editing(e.target)) e.target.blur();
			return;
		}

		// While typing, only the save-comment bindings apply. That is the
		// whole reason Gerrit binds Ctrl/Cmd+Enter.
		if (editing(e.target)) {
			if (direct.get(s) === "saveComment") {
				e.preventDefault();
				actions.saveComment();
			}
			return;
		}

		if (pending) {
			const m = combo.get(pending);
			clearPending();
			const action = m && m.get(e.key);
			if (action) { e.preventDefault(); run(action); }
			return;
		}
		// A shifted letter never arms a combo, which is what keeps G
		// (grab snapshot) out of the way of the g prefix.
		if (combo.has(s) && !e.shiftKey) {
			e.preventDefault();
			pending = s;
			pendingTimer = setTimeout(clearPending, comboTimeout);
			return;
		}
		const action = direct.get(s);
		if (!action) return;
		if (e.repeat && !repeatable.has(s)) return;
		e.preventDefault();
		run(action);
	});

	function clearPending() {
		pending = null;
		if (pendingTimer) { clearTimeout(pendingTimer); pendingTimer = null; }
	}

	function run(name) {
		const fn = actions[name];
		if (fn) fn();
		else console.warn("review: no handler for shortcut action", name);
	}

	// ---- cursor ----------------------------------------------------------

	// The cursor is a row: a change, a file, or a line of a diff.
	function rows() {
		const sel = page === "diff" ? "#diffbody > tr.diffrow" : "tr.item";
		return Array.from(document.querySelectorAll(sel)).filter(visible);
	}

	function visible(el) {
		return el.offsetParent !== null;
	}

	let cursor = -1;

	function setCursor(i, scroll = true) {
		const all = rows();
		if (all.length === 0) return null;
		if (i < 0) i = 0;
		if (i >= all.length) i = all.length - 1;
		for (const r of all) r.classList.remove("cursor");
		cursor = i;
		const row = all[i];
		row.classList.add("cursor");
		if (scroll) row.scrollIntoView({ block: "nearest" });
		return row;
	}

	function currentRow() {
		const all = rows();
		if (cursor < 0 || cursor >= all.length) return null;
		return all[cursor];
	}

	function moveBy(d) {
		if (cursor < 0) { setCursor(d > 0 ? 0 : rows().length - 1); return; }
		setCursor(cursor + d);
	}

	// side is which pane comments and cursors apply to on a diff.
	let side = "new";

	// ---- navigation helpers ----------------------------------------------

	function go(url) { window.location.href = url; }

	function withParams(changes) {
		const u = new URL(window.location.href);
		for (const [k, v] of Object.entries(changes)) {
			if (v === null) u.searchParams.delete(k);
			else u.searchParams.set(k, v);
		}
		go(u.toString());
	}

	function post(url, body) {
		const form = new FormData();
		for (const [k, v] of Object.entries(body || {})) form.append(k, v);
		return fetch(url, { method: "POST", body: form, redirect: "manual" });
	}

	function files() {
		const el = document.getElementById("filedata");
		return el ? JSON.parse(el.textContent) : [];
	}

	function fileIndex(list) {
		return list.findIndex((f) => f.path === view.file);
	}

	// ---- actions ---------------------------------------------------------

	const actions = {
		showHelp() {
			const d = document.getElementById("help");
			if (d.open) d.close(); else d.showModal();
		},

		focusSearch() {
			const s = document.getElementById("search");
			if (s) { s.focus(); s.select(); }
		},

		goRepos() { go("/"); },

		// Up from a change is that repository's change list, not the list
		// of every repository. The change being left is named in the
		// fragment, so that arriving there puts the cursor back on it
		// rather than at the top of the list.
		goChanges() {
			if (!view.repo) { go("/"); return; }
			let u = "/" + encodeURIComponent(view.repo);
			if (view.key) u += "#" + encodeURIComponent(view.key);
			go(u);
		},

		reload() { window.location.reload(); },

		reloadLatest() { withParams({ s: null, base: null }); },

		nextItem() { moveBy(1); },
		prevItem() { moveBy(-1); },

		openItem() {
			const row = currentRow() || setCursor(0);
			if (!row) return;
			if (row.dataset.href) go(row.dataset.href);
		},

		toggleCheckbox() {
			const row = currentRow() || setCursor(0);
			if (!row) return;
			const box = row.querySelector("input.pickbox");
			if (box) box.checked = !box.checked;
		},

		grabSnapshot() {
			const picked = Array.from(document.querySelectorAll("input.pickbox:checked"))
				.map((b) => b.closest("tr").dataset.key);
			const row = currentRow();
			const keys = picked.length ? picked
				: view.key ? [view.key]
				: row && row.dataset.key ? [row.dataset.key] : [];
			const base = view.repo ? "/" + encodeURIComponent(view.repo) : "";
			if (keys.length === 0) { post(base + "/snapshot", {}).then(() => window.location.reload()); return; }
			Promise.all(keys.map((k) => post(base + "/snapshot", { key: k })))
				.then(() => window.location.reload());
		},

		// M on a change: mark the snapshot being viewed reviewed, then go
		// up to the change list. On a diff M means the per-file version of
		// the same idea, the way Gerrit overloads keys per context.
		reviewChange() {
			const b = document.querySelector(".timeline li.sel button.reviewed");
			const done = () => actions.goChanges();
			// Already reviewed, or a change with no snapshot to mark: just
			// go up rather than toggling the mark back off.
			const url = b && !b.classList.contains("on") && b.getAttribute("hx-post");
			if (!url) { done(); return; }
			post(url).then(done, done);
		},

		openPublish() {
			const f = document.querySelector('form[action$="/publish"]');
			if (!f) { showBar("No drafts to publish"); return; }
			f.requestSubmit();
		},

		// ---- file list ---------------------------------------------------

		openFirstFile() {
			if (page === "files") { const r = setCursor(0); if (r) go(r.dataset.href); return; }
			actions.nextFile();
		},

		openLastFile() {
			if (page === "files") { const r = setCursor(rows().length - 1); if (r) go(r.dataset.href); return; }
			actions.prevFile();
		},

		toggleInlineDiff() {
			const row = currentRow() || setCursor(0);
			if (!row) return;
			const next = row.nextElementSibling;
			if (next && next.classList.contains("inlinerow")) { next.remove(); return; }
			if (!row.dataset.inline) return;
			fetch(row.dataset.inline)
				.then((r) => r.text())
				.then((html) => {
					const tr = document.createElement("tr");
					tr.className = "inlinerow";
					tr.innerHTML = '<td colspan="4"></td>';
					tr.firstChild.innerHTML = html;
					row.after(tr);
					if (window.htmx) htmx.process(tr);
				});
		},

		toggleAllInlineDiffs() {
			if (document.querySelector("tr.inlinerow")) {
				document.querySelectorAll("tr.inlinerow").forEach((r) => r.remove());
				setExpandLabel(false);
				return;
			}
			const all = rows();
			all.forEach((_, i) => { setCursor(i, false); actions.toggleInlineDiff(); });
			// The diffs arrive one fetch at a time, so the button says what
			// it will do next rather than waiting for them all to land.
			setExpandLabel(true);
		},

		// ---- diffs -------------------------------------------------------

		// Gerrit's rule for both: the first press that runs off the end of
		// the diff parks the cursor there and offers the next file; a second
		// press within the timeout takes it.
		nextChunk() {
			if (!jumpChunk(1) && atEnd()) chunkEdge("next", "n");
		},

		prevChunk() {
			jumpChunk(-1);
			if (atStart()) chunkEdge("previous", "p");
		},

		nextThread() { jumpThread(1); },
		prevThread() { jumpThread(-1); },

		visibleLine() {
			const all = rows();
			for (let i = 0; i < all.length; i++) {
				const r = all[i].getBoundingClientRect();
				if (r.top >= 0 && r.bottom <= window.innerHeight) { setCursor(i, false); return; }
			}
		},

		newComment() {
			const row = currentRow() || setCursor(0);
			if (!row) return;
			const cells = row.querySelectorAll("td.num");
			// In side-by-side the second gutter is the new side; in unified
			// there is only one.
			const cell = cells.length > 1 && side === "new" ? cells[1] : cells[0];
			const btn = cell && cell.querySelector("button.linenum");
			if (btn) btn.click();
		},

		saveComment() {
			const el = document.activeElement;
			const form = el && el.closest ? el.closest("form") : null;
			if (form) form.requestSubmit();
		},

		expandAllThreads() {
			document.querySelectorAll(".thread").forEach((t) => t.classList.remove("collapsed"));
		},

		collapseAllThreads() {
			document.querySelectorAll(".thread").forEach((t) => t.classList.add("collapsed"));
		},

		toggleThreads() { document.body.classList.toggle("hidethreads"); },

		toggleAllContext() {
			const u = new URL(window.location.href);
			if (u.searchParams.get("context") === "999999") u.searchParams.delete("context");
			else u.searchParams.set("context", "999999");
			go(u.toString());
		},

		// Hiding the left pane collapses its two columns to zero width
		// rather than removing them: the skip and comment rows span all
		// four columns, so removing columns would leave the table with
		// nowhere to put them and the code column no wider than before.
		toggleLeftPane() {
			const hidden = document.body.classList.toggle("hideleft");
			const cols = document.querySelectorAll("#difftable colgroup col");
			for (let i = 0; i < 2 && i < cols.length; i++) {
				cols[i].style.width = hidden ? "0" : "";
			}
		},

		leftPane() { side = "old"; markSide(); },
		rightPane() { side = "new"; markSide(); },

		toggleUnified() {
			const t = document.getElementById("difftable");
			withParams({ unified: t && t.classList.contains("unified") ? "0" : "1" });
		},

		toggleReviewed() {
			if (page === "files") {
				const row = currentRow() || setCursor(0);
				const b = row && row.querySelector("button.reviewed");
				if (b) b.click();
				return;
			}
			const b = document.querySelector("button.reviewed");
			if (b) b.click();
		},

		// Gerrit's M: mark this file reviewed, then go to the next file that
		// is not reviewed yet. Note that n off the end of a diff does not
		// mark anything; it only skips files already marked.
		reviewedNextFile() {
			const b = document.querySelector("button.reviewed");
			const url = b && !b.classList.contains("on") && b.getAttribute("hx-post");
			if (!url) { navUnreviewed("next"); return; }
			const done = () => navUnreviewed("next");
			post(url).then(done, done);
		},

		// Up from a file is its change, with the file named in the
		// fragment so that arriving there lands on it again.
		openFileList() {
			const a = document.getElementById("filelistlink");
			if (!a) return;
			go(view.file ? a.href + "#" + encodeURIComponent(view.file) : a.href);
		},

		nextFile() {
			const a = document.getElementById("nextfile");
			if (a) go(a.href);
		},

		prevFile() {
			const a = document.getElementById("prevfile");
			if (a) go(a.href);
		},

		nextFileWithComments() { jumpCommentedFile(1); },
		prevFileWithComments() { jumpCommentedFile(-1); },

		diffPrefs() {
			const d = document.getElementById("prefs");
			if (d) d.showModal();
		},

		// ---- snapshot selection (Gerrit's v combo) ------------------------

		diffAgainstBase() { withParams({ base: null }); },

		diffAgainstLatest() { withParams({ s: view.latest }); },

		diffBaseAgainstLeft() {
			// Compare the parent with whatever is currently on the left.
			if (!view.base || view.base === "0") return;
			withParams({ base: null, s: view.base });
		},

		diffRightAgainstLatest() {
			if (view.target === view.latest) return;
			withParams({ base: view.target, s: view.latest });
		},

		diffBaseAgainstLatest() { withParams({ base: null, s: view.latest }); },
	};

	// setExpandLabel keeps the button naming what pressing it would do.
	function setExpandLabel(expanded) {
		const b = document.getElementById("expandall");
		if (b) b.textContent = expanded ? "Collapse all" : "Expand all";
	}

	function markSide() {
		document.body.classList.toggle("side-old", side === "old");
		document.body.classList.toggle("side-new", side === "new");
	}

	// jumpChunk moves the cursor to the next or previous changed chunk and
	// reports whether it found one. With no chunk left it clips the cursor
	// to the last or first line, the way Gerrit's cursor does.
	function jumpChunk(d) {
		const all = rows();
		if (all.length === 0) return false;
		let i = cursor < 0 ? (d > 0 ? -1 : all.length) : cursor;
		// Step past the current chunk, then to the start of the next one.
		let seenGap = all[i] === undefined || all[i].classList.contains("equal");
		for (i += d; i >= 0 && i < all.length; i += d) {
			const changed = !all[i].classList.contains("equal");
			if (!changed) { seenGap = true; continue; }
			if (!seenGap) continue;
			// A chunk a rebase brought along is not this change's work and
			// has nothing in it to review, so step over it as if it were
			// unchanged text. Clearing seenGap walks off the rest of the
			// chunk the same way the current one is walked off above.
			if (all[i].classList.contains("rebased")) { seenGap = false; continue; }
			// For backward motion, land on the first row of the chunk.
			if (d < 0) {
				while (i - 1 >= 0 && !all[i - 1].classList.contains("equal")) i--;
			}
			scrollChunk(setCursor(i, false));
			return true;
		}
		scrollChunk(setCursor(d > 0 ? all.length - 1 : 0, false));
		return false;
	}

	// topLines is how far below the top of the page n and p leave the chunk
	// they land on: a margin wide enough to keep the lines just above it in
	// sight, with the rest of the window left for what comes after.
	const topLines = 10;

	// scrollChunk brings a chunk up to the top of the page instead of barely
	// into view. scrollIntoView's "nearest" scrolls as little as it can, so a
	// chunk below the fold ends up sitting on the bottom edge with its own
	// consequences off screen, which is the one place a diff is no use.
	function scrollChunk(row) {
		if (!row) return;
		// The top bar is pinned over the page, so the top of the page is
		// where the bar ends rather than where the document begins.
		const bar = document.querySelector(".topbar");
		const barH = bar ? bar.getBoundingClientRect().height : 0;
		const margin = barH + topLines * lineHeight();
		const at = row.getBoundingClientRect().top;
		// Near enough already: below the bar and no more than topLines down.
		// Scrolling a chunk that is right there only costs the reader the
		// place they had.
		if (at >= barH && at <= margin) return;
		// There is no scrolling past the end of the document, so a chunk near
		// the end stops wherever showing the bottom of the file leaves it.
		const max = Math.max(document.documentElement.scrollHeight - window.innerHeight, 0);
		window.scrollTo(0, Math.min(Math.max(window.scrollY + at - margin, 0), max));
	}

	// A line is one row of the diff, whose height the stylesheet sets; ask
	// for it rather than writing the number down here too.
	function lineHeight() {
		const t = document.getElementById("difftable");
		const h = t ? parseFloat(getComputedStyle(t).lineHeight) : NaN;
		return h > 0 ? h : 18;
	}

	// A file that did not change between the two revisions being compared
	// has no rows to move through at all. Counting that as both ends means
	// n and p offer the next file immediately, rather than doing nothing.
	function atEnd() {
		const all = rows();
		return all.length === 0 || cursor === all.length - 1;
	}

	function atStart() {
		const all = rows();
		return all.length === 0 || cursor === 0;
	}

	// Gerrit gives five seconds to press the key a second time.
	const edgeTimeout = 5000;
	const edgeAt = {};
	const edgeShown = {};
	let barTimer = null;

	function chunkEdge(direction, key) {
		const now = Date.now();
		if (edgeAt[direction] && now - edgeAt[direction] <= edgeTimeout) {
			delete edgeAt[direction];
			hideBar();
			navUnreviewed(direction);
			return;
		}
		edgeAt[direction] = now;
		// Gerrit shows the reminder once per direction per diff view. Each
		// file here is its own page load, so it appears once per file.
		if (edgeShown[direction]) return;
		edgeShown[direction] = true;
		showBar("Press " + key + " again to navigate to " + direction + " unreviewed file");
	}

	function navUnreviewed(direction) {
		const list = files();
		if (list.length === 0) return;
		const step = direction === "next" ? 1 : -1;
		const here = list.findIndex((f) => f.path === view.file);
		if (here < 0) {
			go(step > 0 ? list[0].url : list[list.length - 1].url);
			return;
		}
		// Walk the list from here, wrapping once, and stop at the first
		// file still unreviewed. Wrapping matters because the files left
		// unreviewed are often behind you: the commit message sits first
		// and is easy to leave for last. The file being left is skipped
		// even if the page still lists it as unreviewed, since marking it
		// is what got us here. A file the change does not touch is skipped
		// as well: it is not unreviewed so much as nothing to review.
		for (let n = 1; n <= list.length; n++) {
			const i = (((here + step * n) % list.length) + list.length) % list.length;
			const f = list[i];
			if (f.path === view.file) continue;
			if (!f.reviewed && !f.rebase) { go(f.url); return; }
		}
		// Nothing left to review: up to the change.
		actions.openFileList();
	}

	function showBar(msg) {
		const bar = document.getElementById("navbar");
		if (!bar) return;
		bar.textContent = msg;
		bar.hidden = false;
		clearTimeout(barTimer);
		barTimer = setTimeout(hideBar, edgeTimeout);
	}

	function hideBar() {
		const bar = document.getElementById("navbar");
		if (bar) bar.hidden = true;
		clearTimeout(barTimer);
		barTimer = null;
	}

	function jumpThread(d) {
		const threads = Array.from(document.querySelectorAll(".thread")).filter(visible);
		if (threads.length === 0) return;
		const y = window.scrollY + 1;
		let target = null;
		if (d > 0) target = threads.find((t) => t.getBoundingClientRect().top + window.scrollY > y);
		else target = threads.reverse().find((t) => t.getBoundingClientRect().top + window.scrollY < y - 2);
		if (!target) target = d > 0 ? threads[0] : threads[threads.length - 1];
		target.scrollIntoView({ block: "center" });
		document.querySelectorAll(".thread.focused").forEach((t) => t.classList.remove("focused"));
		target.classList.add("focused");
	}

	function jumpCommentedFile(d) {
		const list = files();
		let i = fileIndex(list);
		if (i < 0) i = d > 0 ? -1 : list.length;
		for (i += d; i >= 0 && i < list.length; i += d) {
			if (list[i].comments > 0) { go(list[i].url); return; }
		}
	}

	// ---- wiring ----------------------------------------------------------

	document.addEventListener("click", function (e) {
		const b = e.target.closest("#helpbutton");
		if (b) { e.preventDefault(); actions.showHelp(); return; }
		const p = e.target.closest("#prefsbutton");
		if (p) { e.preventDefault(); actions.diffPrefs(); return; }
		const x = e.target.closest("#expandall");
		if (x) { e.preventDefault(); actions.toggleAllInlineDiffs(); return; }
		const s = e.target.closest("#showrebase");
		if (s) { e.preventDefault(); toggleRebaseFiles(); return; }
		// Clicking a row moves the cursor there, and anywhere in the bar
		// of a change or a file opens it: the title is a small target for
		// something the whole row stands for. Controls inside the row do
		// their own thing.
		const row = e.target.closest("tr.item, tr.diffrow");
		if (!row) return;
		const i = rows().indexOf(row);
		if (i >= 0) setCursor(i, false);
		if (row.dataset.href && !e.target.closest("a, button, input, label, select, textarea")) {
			go(row.dataset.href);
		}
	});

	// A form that posts and reloads leaves the page looking untouched while
	// the server works, and snapshotting a repository full of changes takes
	// long enough to press the button a second time. Say so in the button
	// itself, and stop it being pressed again; the page is about to be
	// replaced, so nothing has to put it back.
	//
	// Only plain posting forms are caught. The comment forms are htmx's,
	// which have no method of their own and swap in place too quickly to
	// need any of this.
	document.addEventListener("submit", function (e) {
		const form = e.target.closest("form");
		if (!form || form.method.toLowerCase() !== "post") return;
		const b = form.querySelector('button[type="submit"]');
		if (!b || b.disabled) return;
		if (b.dataset.busy) b.textContent = b.dataset.busy;
		b.classList.add("busy");
		// Disabling it now would drop it from the submission in some
		// browsers, so wait until the request is on its way.
		setTimeout(() => { b.disabled = true; }, 0);
	});

	// The filter box hides rows that do not match, like Gerrit's search.
	const search = document.getElementById("search");
	if (search) {
		search.addEventListener("input", function () {
			const q = search.value.toLowerCase();
			document.querySelectorAll("tr.item").forEach(function (row) {
				const hay = (row.dataset.filter || "").toLowerCase();
				row.hidden = q !== "" && !hay.includes(q);
			});
			cursor = -1;
		});
	}

	// toggleRebaseFiles shows or hides the files the change does not touch,
	// which the list leaves out until they are asked for. Rows appearing or
	// disappearing renumbers everything, so the cursor starts over.
	function toggleRebaseFiles() {
		const table = document.querySelector("table.filelist");
		const b = document.getElementById("showrebase");
		if (!table || !b) return;
		const hidden = table.classList.toggle("hiderebase");
		const n = b.dataset.count;
		b.textContent = (hidden ? "show " : "hide ") + n +
			" rebase-only file" + (n === "1" ? "" : "s");
		cursor = -1;
		setCursor(0, false);
	}

	// selectFromHash puts the cursor on the row named in the fragment,
	// which is how coming up from a change or a file lands back on it.
	function selectFromHash() {
		if (!location.hash) return false;
		const want = decodeURIComponent(location.hash.slice(1));
		const named = (r) => r.dataset.key === want || r.dataset.file === want;
		// Coming up from a rebase-only file lands on a row the list is
		// hiding. Having asked to look at that file, the reader should find
		// it where they left it rather than an empty-looking list.
		const hiding = document.querySelector("table.filelist.hiderebase");
		if (hiding && !rows().some(named) &&
			Array.from(hiding.querySelectorAll("tr.rebaseonly")).some(named)) {
			toggleRebaseFiles();
		}
		const i = rows().findIndex(named);
		if (i < 0) return false;
		setCursor(i);
		return true;
	}

	markSide();
	if (page !== "diff" && !selectFromHash()) setCursor(0, false);
})();
