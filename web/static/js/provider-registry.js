// Code generated from internal/channel definitions; DO NOT EDIT.
(() => {
  "use strict";
  const providers = Object.freeze([
    Object.freeze({"key":"workbuddy","label":"WorkBuddy","apiPrefix":"/workbuddy/v1","generic":true,"default":true,"theme":"orange","accountCreate":"browser"}),
    Object.freeze({"key":"qoder","label":"Qoder","apiPrefix":"/qoder/v1","generic":true,"theme":"green","accountCreate":"browser"}),
    Object.freeze({"key":"cline","label":"Cline","apiPrefix":"/cline/v1","generic":true,"theme":"blue","accountCreate":"browser"}),
    Object.freeze({"key":"grok","label":"Grok","apiPrefix":"/grok/v1","generic":false,"theme":"red","accountCreate":"browser"}),
  ]);
  const byKey = Object.freeze(Object.fromEntries(providers.map((item) => [item.key, item])));
  window.OrchidsProviderRegistry = Object.freeze({
    providers,
    keys: Object.freeze(providers.map((item) => item.key)),
    channels: Object.freeze(providers.map((item) => item.label)),
    defaultProviderKey: providers.find((item) => item.default)?.key || "",
    ready: Promise.resolve(),
    get(value) { return byKey[String(value || "").trim().toLowerCase()] || null; },
    label(value) { const raw = String(value || "").trim(); return this.get(raw)?.label || raw; },
  });
})();
