// Turn a rendered Markdown document into a slide deck:
// every <h1> starts a new slide, and anything after a <hr>
// on a slide is speaker notes.
//
// Typing P opens a second window showing just the slides, for
// screen sharing, and turns this window into the presenter view:
// a small copy of the slide above its speaker notes. The two
// windows stay on the same slide, whichever one is driving.

(function() {
"use strict";

var deck = document.getElementById("deck");
var notes = document.getElementById("notes");
var slides = [];
var contents = [];
var noteDivs = [];
var cur = 0;
var overview = false;

// present is true in the window showing just the slides,
// and win is the presentation window opened from this one.
var present = location.search.indexOf("present") >= 0;
var win = null;

// Talk timing: talkSecs comes from a "time: 20m" line at the top of the
// first slide's notes, and the notes are the script, so each slide's
// share of the time is its share of the words.
var talkSecs = 0;
var totalWords = 0;
var slideStart = [];
var slideEnd = [];
var timerStart = null;

var chan = window.BroadcastChannel != null
	? new BroadcastChannel("mdweb-talk:" + location.pathname)
	: null;

// build splits the rendered Markdown into slides at each <h1>.
function build() {
	var nodes = [];
	for (var n = deck.firstChild; n != null; n = n.nextSibling)
		nodes.push(n);

	var body = null;
	var note = null;
	for (var i = 0; i < nodes.length; i++) {
		var n = nodes[i];
		if (body == null || (n.nodeType == 1 && n.tagName == "H1")) {
			body = newSlide();
			note = null;
		}
		// The first rule on a slide starts the speaker notes.
		if (note == null && n.nodeType == 1 && n.tagName == "HR") {
			note = noteDivs[noteDivs.length - 1];
			deck.removeChild(n);
			continue;
		}
		(note != null ? note : body).appendChild(n);
	}
	// Drop a leading slide holding only whitespace.
	if (slides.length > 0 && slides[0].textContent.trim() == "" && noteDivs[0].textContent.trim() == "") {
		deck.removeChild(slides[0].parentNode);
		notes.removeChild(noteDivs[0]);
		slides.shift();
		noteDivs.shift();
	}
	for (var i = 0; i < slides.length; i++) {
		var b = slides[i].firstChild;
		// An empty heading (just "#") makes a slide but takes up no room.
		var h = heading(b);
		if (h != null && h.textContent.trim() == "")
			h.classList.add("empty");
		// A heading-free slide holding only an image is all image.
		if (imageOnly(b)) {
			slides[i].classList.add("image");
			continue;
		}
		// A slide with nothing but its heading is a title slide,
		// and so is the first slide, provided it is only prose.
		var only = b.children.length == 1 && b.children[0].tagName == "H1";
		if (only || (i == 0 && b.querySelector("ul, ol, pre, table, blockquote, img") == null))
			slides[i].classList.add("title");
	}
	// Everything below the heading is shrunk to fit as a unit,
	// so that a crowded slide keeps its heading at full size.
	for (var i = 0; i < slides.length; i++)
		contents[i] = splitContent(slides[i].firstChild);
}

// splitContent moves everything below body's heading into a
// slide-content div, and returns it.
function splitContent(body) {
	var content = document.createElement("div");
	content.className = "slide-content";
	var h = heading(body);
	var n = h != null ? h.nextSibling : body.firstChild;
	while (n != null) {
		var next = n.nextSibling;
		content.appendChild(n);
		n = next;
	}
	body.appendChild(content);
	return content;
}

function heading(body) {
	var h = body.firstElementChild;
	return h != null && h.tagName == "H1" ? h : null;
}

// imageOnly reports whether body holds a single image and no heading or text.
function imageOnly(body) {
	var h = heading(body);
	if (h != null && h.textContent.trim() != "")
		return false;
	return body.querySelectorAll("img").length == 1 && body.textContent.trim() == "";
}

function newSlide() {
	var wrap = document.createElement("div");
	wrap.className = "slide-wrap";
	var slide = document.createElement("section");
	slide.className = "slide";
	var body = document.createElement("div");
	body.className = "slide-body";
	slide.appendChild(body);
	wrap.appendChild(slide);
	deck.appendChild(wrap);
	slides.push(slide);

	// Speaker notes live outside the slide, in their own panel.
	var note = document.createElement("div");
	note.className = "slide-notes";
	notes.appendChild(note);
	noteDivs.push(note);

	return body;
}

// layout scales the deck to fit the window, and shrinks any
// slide whose content overflows its box.
function layout() {
	var scale = Math.min(window.innerWidth / 1280, window.innerHeight / 720);
	document.documentElement.style.setProperty("--scale", scale);
	document.documentElement.style.setProperty("--thumb", 0.2);
	// In the presenter view the slide is a small copy above the notes.
	var note = Math.min((window.innerWidth - 64) / 1280, window.innerHeight * 0.4 / 720);
	document.documentElement.style.setProperty("--note", note);
	for (var i = 0; i < slides.length; i++)
		fit(slides[i], contents[i]);
}

// fit shrinks the content below a slide's heading until it fits.
// The heading itself is never scaled: on a crowded slide the words
// get smaller but the title stays put.
function fit(slide, content) {
	if (slide.classList.contains("image") || content == null)
		return;
	var body = slide.firstChild;
	var style = window.getComputedStyle(slide);
	var f = 1;
	slide.style.setProperty("--fit", f);
	var avail = slide.clientHeight - parseFloat(style.paddingTop) -
		parseFloat(style.paddingBottom) - (content.offsetTop - body.offsetTop);
	// Scaling the content makes it wider, which reflows text and changes
	// its height, so converge on a scale instead of computing one.
	for (var i = 0; i < 20 && content.scrollHeight * f > avail + 0.5; i++) {
		f = Math.max(0.4, Math.min(f * avail / (content.scrollHeight * f), f - 0.02));
		slide.style.setProperty("--fit", f);
	}
}

// schedule reads the talk time and divides it among the slides.
function schedule() {
	if (noteDivs.length == 0)
		return;
	talkSecs = takeTime(noteDivs[0]);
	var words = [];
	for (var i = 0; i < noteDivs.length; i++) {
		words[i] = countWords(noteDivs[i].textContent);
		totalWords += words[i];
	}
	var sum = 0;
	for (var i = 0; i < noteDivs.length; i++) {
		slideStart[i] = totalWords > 0 ? talkSecs * sum / totalWords : 0;
		sum += words[i];
		slideEnd[i] = totalWords > 0 ? talkSecs * sum / totalWords : 0;
	}
}

// takeTime removes a leading "time: 20m" line from note,
// returning the duration it names, in seconds.
function takeTime(note) {
	var el = note.firstElementChild;
	if (el == null)
		return 0;
	var m = /^[ \t]*time:[ \t]*([^\n]*)/i.exec(el.textContent);
	if (m == null)
		return 0;
	var secs = parseDuration(m[1]);
	if (secs <= 0)
		return 0;
	var t = firstTextNode(el);
	var i = t.data.indexOf("\n");
	if (i < 0)
		el.parentNode.removeChild(el);
	else
		t.data = t.data.substr(i + 1);
	return secs;
}

// parseDuration parses a duration like "20m", "1h30m", or "90s".
// A number with no unit is minutes.
function parseDuration(s) {
	var re = /([0-9]+(?:\.[0-9]+)?)\s*([a-z]*)/gi;
	var secs = 0;
	var m;
	while ((m = re.exec(s)) != null) {
		var u = m[2].toLowerCase().charAt(0);
		secs += parseFloat(m[1]) * (u == "h" ? 3600 : u == "s" ? 1 : 60);
	}
	return secs;
}

function firstTextNode(el) {
	for (var n = el.firstChild; n != null; n = n.nextSibling) {
		if (n.nodeType == 3)
			return n;
		if (n.nodeType == 1) {
			var t = firstTextNode(n);
			if (t != null)
				return t;
		}
	}
	return null;
}

function countWords(s) {
	var w = s.match(/\S+/g);
	return w == null ? 0 : w.length;
}

// tick updates the clock, the timer, and the schedule for this slide.
function tick() {
	if (!document.body.classList.contains("notes"))
		return;
	var now = new Date();
	clockSpan.textContent = now.toLocaleTimeString();

	var secs = timerStart == null ? 0 : (now.getTime() - timerStart) / 1000;
	elapsedSpan.textContent = fmtDur(secs);
	elapsedSpan.className = "";
	if (talkSecs > 0 && timerStart != null) {
		if (secs > slideEnd[cur])
			elapsedSpan.className = "late";
		else if (secs < slideStart[cur])
			elapsedSpan.className = "ahead";
	}

	if (talkSecs > 0)
		targetSpan.textContent = "slide " + (cur + 1) + " of " + slides.length +
			": " + fmtDur(slideStart[cur]) + "–" + fmtDur(slideEnd[cur]) +
			" of " + fmtDur(talkSecs);
	else
		targetSpan.textContent = "slide " + (cur + 1) + " of " + slides.length;

	// The first slide reports the pace the whole talk assumes.
	wpmSpan.textContent = cur == 0 && talkSecs > 0
		? Math.round(totalWords / (talkSecs / 60)) + " wpm, " + totalWords + " words"
		: "";
}

function fmtDur(secs) {
	secs = Math.max(0, Math.round(secs));
	var m = Math.floor(secs / 60);
	var h = Math.floor(m / 60);
	var s = pad(secs % 60);
	return h > 0 ? h + ":" + pad(m % 60) + ":" + s : m + ":" + s;
}

function pad(n) {
	return n < 10 ? "0" + n : "" + n;
}

function resetTimer() {
	timerStart = new Date().getTime();
	tick();
}

// show displays slide i. If quiet is true, the change came from
// another window and is not sent back out.
function show(i, quiet) {
	if (i < 0)
		i = 0;
	if (i >= slides.length)
		i = slides.length - 1;
	if (slides.length == 0)
		return;
	slides[cur].parentNode.classList.remove("current");
	noteDivs[cur].classList.remove("current");
	cur = i;
	slides[cur].parentNode.classList.add("current");
	noteDivs[cur].classList.add("current");
	notes.scrollTop = 0;
	document.getElementById("pageno").textContent = (cur + 1) + " / " + slides.length;
	if (parseInt(location.hash.substr(1)) != cur + 1)
		history.replaceState(null, "", "#" + (cur + 1));
	if (overview)
		slides[cur].parentNode.scrollIntoView({block: "nearest"});
	tick();
	if (!quiet)
		send({slide: cur});
}

function send(msg) {
	if (chan != null)
		chan.postMessage(msg);
}

if (chan != null) {
	chan.onmessage = function(e) {
		var m = e.data;
		if (m.slide != null && m.slide != cur)
			show(m.slide, true);
		if (m.reset) {
			timerStart = m.reset;
			tick();
		}
		// The presentation window is gone: stop presenting.
		if (m.bye && !present)
			setNotes(false);
	};
}

// startPresent opens a window showing just the slides, to be shared
// on screen, and turns this window into the presenter view. Typing P
// again, or closing either window, ends the presentation.
function startPresent() {
	if (present) {
		window.close();
		return;
	}
	if (win != null && !win.closed) {
		win.close();
		endPresent();
		return;
	}
	var w = Math.min(screen.availWidth, 1280);
	var h = Math.round(w * 720 / 1280);
	win = window.open(location.pathname + "?present#" + (cur + 1), "mdweb-present",
		"popup=1,fullscreen=1,width=" + w + ",height=" + h +
		",left=" + Math.round((screen.availWidth - w) / 2) +
		",top=" + Math.round((screen.availHeight - h) / 2));
	if (win == null) {
		toast("pop-up blocked: allow pop-up windows to present");
		return;
	}
	setNotes(true);
	// pagehide is not guaranteed, so watch for the window closing too.
	var t = setInterval(function() {
		if (win == null || win.closed) {
			clearInterval(t);
			endPresent();
		}
	}, 500);
}

function isFullscreen() {
	return (document.fullscreenElement || document.webkitFullscreenElement) != null;
}

function enterFullscreen() {
	var el = document.documentElement;
	var f = el.requestFullscreen || el.webkitRequestFullscreen;
	if (f != null) {
		var p = f.call(el);
		if (p != null && p.catch != null)
			p.catch(function() {});
	}
}

function exitFullscreen() {
	var f = document.exitFullscreen || document.webkitExitFullscreen;
	if (f != null)
		f.call(document);
}

// goFullscreen tries to lose the browser's window frame, which is the
// only way a popup window can hide its title and location bars.
// Browsers grant full screen only to a user gesture, and the gesture
// that opened this window may not count, so if the attempt fails,
// fall back to the next click or key press.
function goFullscreen() {
	enterFullscreen();
	setTimeout(function() {
		if (isFullscreen())
			return;
		toast("click for full screen");
		var f = function() {
			document.removeEventListener("click", f, true);
			document.removeEventListener("keydown", f, true);
			enterFullscreen();
		};
		document.addEventListener("click", f, true);
		document.addEventListener("keydown", f, true);
	}, 500);
}

// toast shows a message for a few seconds.
function toast(text) {
	var msg = document.getElementById("msg");
	msg.textContent = text;
	msg.classList.add("show");
	setTimeout(function() { msg.classList.remove("show"); }, 3000);
}

function endPresent() {
	win = null;
	setNotes(false);
}

function setNotes(on) {
	document.body.classList.toggle("notes", on);
	layout();
	if (on) {
		notes.scrollTop = 0;
		if (timerStart == null)
			resetTimer();
	}
	tick();
}

function setOverview(on) {
	overview = on;
	document.body.classList.toggle("overview", on);
	if (on)
		slides[cur].parentNode.scrollIntoView({block: "center"});
}

function fromHash() {
	var n = parseInt(location.hash.substr(1));
	show(isNaN(n) ? 0 : n - 1, false);
}

document.addEventListener("keydown", function(e) {
	if (e.metaKey || e.ctrlKey || e.altKey)
		return;
	var k = e.key;
	if (e.shiftKey && (k == "p" || k == "r"))  // some browsers report the unshifted key
		k = k.toUpperCase();
	if (document.body.classList.contains("help") && k != "?" && k != "/") {
		document.body.classList.remove("help");
		e.preventDefault();
		return;
	}
	switch (k) {
	case "ArrowRight": case "ArrowDown": case "PageDown": case " ":
	case "n": case "j": case "Enter":
		show(cur + 1);
		break;
	case "ArrowLeft": case "ArrowUp": case "PageUp": case "Backspace":
	case "p": case "k":
		show(cur - 1);
		break;
	case "Home":
		show(0);
		break;
	case "End":
		show(slides.length - 1);
		break;
	case "f":
		if (isFullscreen())
			exitFullscreen();
		else
			enterFullscreen();
		break;
	case "o": case "Escape":
		if (k == "Escape" && !overview)
			return;
		setOverview(!overview);
		break;
	case "P":
		startPresent();
		break;
	case "r": case "R":
		resetTimer();
		send({reset: timerStart});
		break;
	case "?": case "/":
		document.body.classList.toggle("help");
		break;
	default:
		return;
	}
	e.preventDefault();
});

document.addEventListener("click", function(e) {
	if (e.target.closest("a") || e.target.closest("#notes"))
		return;
	if (overview) {
		var wrap = e.target.closest(".slide-wrap");
		if (wrap == null)
			return;
		show(slides.indexOf(wrap.firstChild));
		setOverview(false);
		return;
	}
	if (document.body.classList.contains("help")) {
		document.body.classList.remove("help");
		return;
	}
	show(cur + (e.clientX < window.innerWidth / 4 ? -1 : 1));
});

var touchX = null;
document.addEventListener("touchstart", function(e) { touchX = e.touches[0].clientX; });
document.addEventListener("touchend", function(e) {
	if (touchX == null || overview)
		return;
	var dx = e.changedTouches[0].clientX - touchX;
	if (Math.abs(dx) > 40)
		show(cur + (dx < 0 ? 1 : -1));
	touchX = null;
});

window.addEventListener("pagehide", function() {
	if (present)
		send({bye: 1});
});

window.addEventListener("resize", layout);
window.addEventListener("hashchange", fromHash);
// Web fonts can land after the first layout and change the metrics.
if (document.fonts != null)
	document.fonts.ready.then(layout);

var status = document.getElementById("status");
var clockSpan = span("clock");
var elapsedSpan = span("elapsed");
var targetSpan = span("target");
var wpmSpan = span("wpm");

function span(id) {
	var el = document.createElement("span");
	el.id = id;
	status.appendChild(el);
	return el;
}

build();
schedule();
layout();
setInterval(tick, 500);
fromHash();
deck.classList.add("ready");

// A presentation window opened by another window shows only the slides,
// so drop the browser frame around them if we possibly can.
if (present && window.opener != null)
	goFullscreen();

})();
