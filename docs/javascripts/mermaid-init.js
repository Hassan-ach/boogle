// Mermaid diagrams are rendered client-side from ```mermaid fences.
// Material fires document$ after each navigation, so this runs again for
// instantly loaded pages and for the initial page alike.
document$.subscribe(() => {
  if (!window.mermaid) return;

  const dark = document.documentElement.getAttribute("data-theme") === "slate";
  window.mermaid.initialize({
    startOnLoad: false,
    securityLevel: "strict",
    theme: dark ? "dark" : "default",
    flowchart: { curve: "basis" },
  });
  window.mermaid.run({ querySelector: ".mermaid" });
});
