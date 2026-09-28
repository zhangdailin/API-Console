// Common JavaScript functions

function setSidebarAccountStats(total, normal, abnormal) {
  const footerTotal = document.getElementById("footerTotal");
  if (footerTotal) footerTotal.textContent = String(total || 0);

  const footerNormal = document.getElementById("footerNormal");
  if (footerNormal) footerNormal.textContent = String(normal || 0);

  const footerAbnormal = document.getElementById("footerAbnormal");
  if (footerAbnormal) footerAbnormal.textContent = String(abnormal || 0);
}

function normalizeSidebarAccountType(acc) {
  return String(acc?.account_type || "").toLowerCase();
}

function normalizeSidebarStatusCode(statusCode) {
  if (statusCode === null || statusCode === undefined) return "";
  return String(statusCode).trim();
}

// Identify Grok Build OAuth for its channel-specific quota display.
function isSidebarGrokOAuthAccount(acc) {
  return normalizeSidebarAccountType(acc) === "grok" &&
    String(acc?.credential_type || "").trim().toLowerCase() === "oauth";
}

function hasSidebarAccountCredential(acc) {
  return acc?.has_credential === true;
}

function getSidebarQuotaStats(acc) {
  if (!acc) return null;
  const type = normalizeSidebarAccountType(acc);
  if (type === "workbuddy") {
    // The credit meter reports the remaining share of the current cycle, so the
    // explicit quota_* fields are authoritative and usage_current must not be
    // read as "used".
    const limit = Math.max(0, Number(acc.quota_limit || 0));
    const remaining = Math.max(0, Number(acc.quota_remaining || 0));
    if (acc.quota_supported === true && limit > 0) {
      return {
        supported: true,
        limit,
        remaining,
        unit: acc.quota_unit || "credits",
        plan: acc.quota_plan || "",
        resetAt: acc.quota_reset_at || "",
        packageRemaining: Math.max(0, Number(acc.quota_package_remaining || 0)),
      };
    }
    return null;
  }
  if (type === "qoder") {
    // The channel reads the credit window from the gateway's own quota endpoint,
    // so the explicit quota_* fields are authoritative and usage_current must not
    // be read as a balance. Without a snapshot the answer is "unknown" rather
    // than a fabricated zero.
    const limit = Math.max(0, Number(acc.quota_limit || 0));
    const remaining = Math.max(0, Number(acc.quota_remaining || 0));
    if (acc.quota_supported === true) {
      return {
        supported: true,
        limit,
        remaining,
        unit: acc.quota_unit || "credits",
        plan: acc.quota_plan || "",
        resetAt: acc.quota_reset_at || "",
      };
    }
    return null;
  }
  const explicitLimit = Math.floor(acc.quota_limit || 0);
  const hasExplicitRemaining = acc.quota_remaining !== undefined && acc.quota_remaining !== null;
  if (explicitLimit > 0 && hasExplicitRemaining) {
    return {
      supported: true,
      limit: explicitLimit,
      remaining: Math.max(0, Math.floor(acc.quota_remaining || 0)),
      // The server labels an inferred window. Carrying the label here as well keeps a
      // caller from rendering an estimate as a balance just because it took this
      // shortcut instead of the channel-specific branch.
      estimated: acc.quota_confidence === "estimated",
      limitKnown: acc.quota_limit_known === true,
    };
  }

  const limit = Math.floor(acc.usage_limit || 0);
  if (limit <= 0) return null;
  const current = Math.floor(acc.usage_current || 0);
  const remaining = Math.max(0, current);
  return { supported: true, limit, remaining };
}

function isQuotaOnlyStatus(acc) {
  if (!acc) return false;
  const type = normalizeSidebarAccountType(acc);
  const quota = getSidebarQuotaStats(acc);
  const statusCode = normalizeSidebarStatusCode(acc.status_code);
  // An exhausted allowance is a business limit on every channel, not a fault:
  // the credential is intact and the scheduler resumes the account when the
  // window resets. The old channel allowlist here meant a Grok window that ran
  // out was reported as 异常 on one page and as 额度不足 on another.
  if (isQuotaExhaustedQuota(acc, quota)) return true;
  if (statusCode === "402") return true;
  if (type === "qoder") {
    // Qoder reports the window share directly and the gateway's exhausted verdict
    // is authoritative. The limit may legitimately be 0 while credits remain, so
    // the verdict must not be gated on limit > 0.
    return Boolean(acc.quota_exhausted === true || (quota && quota.remaining <= 0 && acc.quota_supported === true));
  }
  return false;
}

// isQuotaExhaustedQuota answers the one question the sidebar and the account
// table must agree on: has this account's allowance run out? It deliberately
// ignores quota_confidence, because an inferred window that reads zero is still
// a drained window, and calling it 异常 made the same account look healthy on
// the operations pages and broken on the accounts page.
function isQuotaExhaustedQuota(acc, quota) {
  if (!acc) return false;
  const stats = quota === undefined ? getSidebarQuotaStats(acc) : quota;
  if (acc.quota_exhausted === true && acc.quota_supported === true) return true;
  return Boolean(stats && stats.limit > 0 && stats.remaining <= 0);
}

function isSidebarAccountAbnormal(acc) {
  if (!acc || !acc.enabled) return true;

  if (isQuotaOnlyStatus(acc)) {
    return false;
  }

  if (normalizeSidebarStatusCode(acc.status_code)) {
    return true;
  }

  return !hasSidebarAccountCredential(acc);
}

function computeSidebarAccountStats(accounts) {
  const list = Array.isArray(accounts) ? accounts : [];
  const total = list.length;
  const abnormal = list.filter(isSidebarAccountAbnormal).length;
  const normal = Math.max(0, total - abnormal);
  return { total, normal, abnormal };
}

async function refreshSidebarAccountStats() {
  if (!document.getElementById("footerTotal")) return;
  if (document.getElementById("accountsList") && document.getElementById("totalAccounts")) return;

  try {
    const res = await fetch("/api/accounts");
    if (!res.ok) return;
    const accounts = await res.json();
    const stats = computeSidebarAccountStats(accounts);
    setSidebarAccountStats(stats.total, stats.normal, stats.abnormal);
  } catch (err) {
    console.debug("Failed to refresh sidebar account stats:", err);
  }
}

function setSidebarOpen(open) {
  const sidebar = document.getElementById("sidebar");
  const overlay = document.querySelector(".sidebar-overlay");
  const menuBtn = document.querySelector(".mobile-menu-btn");
  const shouldOpen = Boolean(open);
  const isMobile = typeof window.matchMedia === "function" && window.matchMedia("(max-width: 900px)").matches;
  if (sidebar) {
    sidebar.classList.toggle("mobile-open", shouldOpen);
    sidebar.setAttribute("aria-hidden", isMobile && !shouldOpen ? "true" : "false");
  }
  if (overlay) {
    overlay.classList.toggle("active", shouldOpen);
    overlay.setAttribute("aria-hidden", shouldOpen ? "false" : "true");
  }
  if (menuBtn) {
    menuBtn.setAttribute("aria-expanded", shouldOpen ? "true" : "false");
  }
  document.body.classList.toggle("sidebar-open", shouldOpen);
}

function toggleSidebar(forceOpen) {
  const sidebar = document.getElementById("sidebar");
  if (!sidebar) return;
  if (typeof forceOpen === "boolean") {
    setSidebarOpen(forceOpen);
    return;
  }
  setSidebarOpen(!sidebar.classList.contains("mobile-open"));
}

// Show toast notification
function showToast(msg, type = 'success') {
  let container = document.getElementById("toastContainer");
  if (!container) {
    container = document.createElement("div");
    container.id = "toastContainer";
    container.className = "toast-container";
    document.body.appendChild(container);
  }
  container.setAttribute("aria-live", type === "error" ? "assertive" : "polite");
  container.setAttribute("aria-atomic", "true");

  const visibleToasts = Array.from(container.querySelectorAll(".toast"));
  while (visibleToasts.length >= 4) {
    visibleToasts.shift()?.remove();
  }

  const toast = document.createElement("div");
  toast.className = "toast";
  toast.dataset.type = type;
  toast.setAttribute("role", type === "error" ? "alert" : "status");
  const iconSpan = document.createElement("span");
  iconSpan.className = "toast-mark";
  iconSpan.setAttribute("aria-hidden", "true");
  iconSpan.textContent = type === "success" ? "OK" : type === "info" ? "i" : "!";
  toast.appendChild(iconSpan);
  const message = document.createElement("span");
  message.className = "toast-message";
  message.textContent = String(msg || "");
  toast.appendChild(message);
  container.appendChild(toast);
  requestAnimationFrame(() => toast.classList.add("show"));
  setTimeout(() => {
    toast.classList.remove("show");
    setTimeout(() => toast.remove(), 400);
  }, 2800);
}

// Copy text to clipboard
async function copyToClipboard(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
    } else {
      const el = document.createElement('textarea');
      el.value = text;
      document.body.appendChild(el);
      el.select();
      document.execCommand('copy');
      document.body.removeChild(el);
    }
    showToast("已复制到剪切板");
  } catch (err) {
    showToast("复制失败", "error");
  }
}

// Logout function
async function logout() {
  if (confirm("确定要退出登录吗？")) {
    try {
      await fetch("/api/logout", { method: "POST" });
      window.location.href = "./login.html";
    } catch (err) {
      window.location.href = "./login.html";
    }
  }
}

// Every switch in the console is a <label class="toggle"> wrapping a checkbox, and the
// paint comes from the .active class. Pages that render a switch without setting that
// class leaned on the .toggle:has(input:checked) fallback in the stylesheet — which
// browsers without :has() ignore, so the switch showed OFF for an enabled row. The
// account form's 启用账号 switch was exactly that case: checked, no class.
//
// Keeping the class in step here gives the console one contract for its switches
// instead of one per page, and keeps the fallback as a fallback.
function syncToggleStates(root) {
  const scope = root && typeof root.querySelectorAll === "function"
    ? root
    : (typeof document !== "undefined" && typeof document.querySelectorAll === "function" ? document : null);
  if (!scope) return;
  scope.querySelectorAll("label.toggle").forEach((label) => {
    const input = label.querySelector("input[type=checkbox]");
    if (input) label.classList.toggle("active", !!input.checked);
  });
}

// A switch flipped by a click or a tap. This is the path every page already takes.
document.addEventListener("change", (event) => {
  const input = event.target;
  if (!input || input.type !== "checkbox") return;
  const label = input.closest ? input.closest("label.toggle") : null;
  if (label) label.classList.toggle("active", !!input.checked);
});

// A switch assigned by script fires no change event — the account form assigns #enabled
// when its dialog opens — so those call sites run syncToggleStates() explicitly rather
// than the console listening to every click on the document: another document-level click
// listener would sit in front of every page's own delegate.
window.syncToggleStates = syncToggleStates;

document.addEventListener("DOMContentLoaded", () => {
  setSidebarOpen(false);
  refreshSidebarAccountStats();
  syncToggleStates();

  document.querySelector("[data-sidebar-toggle]")?.addEventListener("click", () => toggleSidebar());
  document.querySelector("[data-sidebar-close]")?.addEventListener("click", () => setSidebarOpen(false));
  document.querySelector("[data-logout]")?.addEventListener("click", logout);

  const sidebarMedia = typeof window.matchMedia === "function" ? window.matchMedia("(max-width: 900px)") : null;
  const resetSidebar = () => setSidebarOpen(false);
  if (sidebarMedia?.addEventListener) sidebarMedia.addEventListener("change", resetSidebar);
  else if (sidebarMedia?.addListener) sidebarMedia.addListener(resetSidebar);

  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      setSidebarOpen(false);
    }
  });
});

// Centralized utility helper functions
// escapeHtml is on the hot path of every table render (the account list calls it
// per cell per row), so it maps the five entities directly instead of building a
// detached <div> for every value: that allocated one element per cell and forced
// a serialization round-trip for text that is already a string.
const HTML_ESCAPE_MAP = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
const HTML_ESCAPE_RE = /[&<>"']/g;

function escapeHtml(text) {
  if (text === null || text === undefined) return "";
  return String(text).replace(HTML_ESCAPE_RE, (ch) => HTML_ESCAPE_MAP[ch]);
}

function encodeData(value) {
  return encodeURIComponent(value === null || value === undefined ? "" : String(value));
}

function decodeData(value) {
  if (!value) return "";
  try {
    return decodeURIComponent(value);
  } catch (err) {
    return value;
  }
}

function formatTime(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "-";
  const now = new Date();
  const diff = (now - d) / 1000;
  if (diff < 60) return "刚刚";
  if (diff < 3600) return Math.floor(diff / 60) + " 分钟前";
  if (diff < 86400) return Math.floor(diff / 3600) + " 小时前";
  return d.toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}
