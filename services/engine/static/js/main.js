import Alpine from "alpinejs";
window.Alpine = Alpine;

window.htmx = require("htmx.org");

document.addEventListener("DOMContentLoaded", () => {
    Alpine.start();
});

console.log("Boogle Search Engine initialized");
