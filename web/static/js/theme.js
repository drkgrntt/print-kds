// Loaded as a plain blocking <script> at the top of <head> (see
// internal/views/layout.templ) so [data-theme] is set before first paint.
// With nothing saved, tokens.css follows the OS preference on its own; only
// an explicit choice needs the attribute.
(function () {
	var saved = null;
	try { saved = localStorage.getItem("theme"); } catch (e) {}
	if (saved === "light" || saved === "dark") {
		document.documentElement.setAttribute("data-theme", saved);
	}
})();

function currentEffectiveTheme() {
	var attr = document.documentElement.getAttribute("data-theme");
	if (attr === "light" || attr === "dark") return attr;
	return window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

function toggleTheme() {
	var next = currentEffectiveTheme() === "dark" ? "light" : "dark";
	document.documentElement.setAttribute("data-theme", next);
	try { localStorage.setItem("theme", next); } catch (e) {}
}
