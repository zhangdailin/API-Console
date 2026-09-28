// Accounts management JavaScript

let accounts = [];
let currentPlatform = '';
let accountHealth = {};
let pageSize = 20;
let currentPage = 1;

// DOM 缓存
const domCache = {
    accountsList: null,
    paginationInfo: null,
    paginationControls: null,
};

function initDOMCache() {
    domCache.accountsList = document.getElementById("accountsList");
    domCache.paginationInfo = document.getElementById("paginationInfo");
    domCache.paginationControls = document.getElementById("paginationControls");
}

// Load accounts from API
async function loadAccounts() {
  try {
    const res = await fetch("/api/accounts");
    if (res.status === 401) {
      window.location.href = "./login.html";
      return;
    }
    const loadedAccounts = await res.json();
    accounts = (Array.isArray(loadedAccounts) ? loadedAccounts : []).filter((account) => {
      if (String(account?.account_type || "").trim().toLowerCase() !== "grok") return true;
      return String(account?.credential_type || "").trim().toLowerCase() === "oauth" ||
        String(account?.grok_provider || "").trim().toLowerCase() === "build";
    });
    sortAccounts();
    renderPlatformTabs();
    renderAccounts();
    updateStats();
    // Loading the management page is read-only. Upstream checks can rotate
    // credentials, alter quota/status fields, and re-render rows one by one, so
    // they must only run after the operator clicks the account's refresh action.
  } catch (err) {
    console.error("Failed to load accounts:", err);
    showToast("加载账号失败", "error");
  }
}

// Sort accounts (Default by ID desc)
function sortAccounts() {
  accounts.sort((a, b) => b.id - a.id);
}

// Normalize account type
function normalizeAccountType(acc) {
  return normalizeSidebarAccountType(acc);
}

// quotaProvenance reads where an account's quota number came from and how far it can
// be trusted. The server publishes this next to every quota value (quota_type /
// quota_source / quota_confidence / quota_limit_known), because "未知" alone cannot
// distinguish a paid account whose numeric window upstream does not publish, an
// account inferred as Free, and one that was never synced.
function quotaProvenance(acc) {
  return {
    type: String(acc?.quota_type || ""),
    source: String(acc?.quota_source || ""),
    confidence: String(acc?.quota_confidence || ""),
    limitKnown: acc?.quota_limit_known === true,
    observed: acc?.quota_observed === true,
    windowHours: Number(acc?.quota_window_hours || 0) || 0,
    note: String(acc?.quota_note || ""),
  };
}

// QUOTA_SOURCE_LABELS names each provenance signal for the operator tooltip: the raw
// id is an implementation detail.
const QUOTA_SOURCE_LABELS = {
  upstreamBilling: "上游账单/额度接口",
  upstreamRateLimit: "上游限流响应头",
  planMetadata: "官方套餐标记",
  billingProfile: "Free 账单画像推断",
  subscription: "官方套餐名",
  upstreamExhaustion: "上游额度耗尽实报",
  unknown: "尚未同步",
};

const QUOTA_CONFIDENCE_LABELS = {
  confirmed: "已确认",
  observed: "实测",
  estimated: "估算",
};

// quotaTooltip explains a quota cell: what the number means, where it came from and
// how much it is worth. An estimate must never read like a balance.
function quotaTooltip(acc, quota) {
  const provenance = quotaProvenance(acc);
  const parts = [];
  if (quota?.estimated) {
    parts.push("Free（推断）估算额度");
  } else if (quota?.confirmedFree) {
    parts.push("Free 实报额度");
  } else if (provenance.type === "free") {
    parts.push("Free");
  } else if (provenance.type === "paid") {
    parts.push("付费额度");
  }
  if (quota?.confirmedFree) {
    parts.push("上游额度耗尽时返回的真实 actual/limit，非估算");
  }
  if (provenance.source) parts.push("来源: " + (QUOTA_SOURCE_LABELS[provenance.source] || provenance.source));
  if (provenance.confidence) parts.push("置信度: " + (QUOTA_CONFIDENCE_LABELS[provenance.confidence] || provenance.confidence));
  if (quota?.estimated && !provenance.limitKnown) {
    parts.push("限额未经上游确认，数字仅供估算");
  }
  if (quota?.estimated && !provenance.observed) {
    parts.push("用量: 本网关未统计到窗口内用量，仅显示估算上限");
  }
  if (quota?.windowHours) parts.push("窗口: 滚动 " + quota.windowHours + " 小时");
  if (provenance.note) parts.push(provenance.note);
  return parts.join(" · ");
}

function getQuotaStats(acc) {
  if (!acc) return null;
  const type = normalizeAccountType(acc);
  if (type === "cline") {
    // Cline publishes no numeric allowance: the recommended-models feed is a
    // list and the inference cap is a rate limit written in prose. Returning
    // null made the 配额 cell fall through to a bare dash, which reads as
    // "nothing was ever read" rather than "this channel is unmetered".
    return {
      supported: false,
      unmetered: true,
      limit: 0,
      remaining: 0,
      used: 0,
      pctRemaining: 0,
      note: String(acc.quota_note || "").trim(),
      modelCount: Array.isArray(acc.cline_model_ids) ? acc.cline_model_ids.length : 0,
    };
  }
  if (type === "workbuddy" || type === "qoder") {
    // Both channels use quota_* snapshots rather than generic usage columns.
    // WorkBuddy derives spent credits from the meter; Qoder reports quota_used.
    const base = getSidebarQuotaStats(acc);
    if (!base) return { supported: false, unknown: true, limit: 0, remaining: 0, used: 0, pctRemaining: 0 };
    const limit = Math.max(0, base.limit || 0);
    const remaining = Math.max(0, base.remaining || 0);
    const qoder = type === "qoder";
    return {
      ...base,
      limit,
      remaining,
      used: qoder ? Math.max(0, Number(acc.quota_used || 0)) : Math.max(0, limit - remaining),
      pctRemaining: limit > 0 ? Math.min(100, Math.round((remaining / limit) * 100)) : 0,
      ...(qoder ? {
        qoder: true,
        // Gateway exhaustion is authoritative even before counters refresh.
        exhausted: acc.quota_exhausted === true,
        plan: base.plan || "",
        upgradeUrl: String(acc.quota_upgrade_url || "").trim(),
      } : { workbuddy: true, packageRemaining: base.packageRemaining || 0 }),
      unit: base.unit || "credits",
      resetAt: base.resetAt || "",
    };
  }
  // Build billing and response throttling are different xAI products. Never
  // use request/token rate-limit headers as a paid-plan balance.
  if (type === "grok" && isSidebarGrokOAuthAccount(acc)) {
    const weekly = acc.grok_billing && acc.grok_billing.weekly;
    if (weekly && weekly.has_usage === true && Number.isFinite(Number(weekly.usage_percent))) {
      const used = Math.max(0, Math.min(100, Number(weekly.usage_percent)));
      return {
        supported: true,
        limit: 100,
        remaining: Math.max(0, 100 - used),
        used,
        pctRemaining: Math.max(0, 100 - used),
        weeklyPercent: true,
        resetAt: weekly.reset_at || "",
      };
    }
    // No upstream window. A Free account is the common case here, and saying "未知"
    // for it throws away the one thing an operator wants to know: it is Free, and the
    // window is roughly this big. The estimate is labelled as such (≈ plus the
    // provenance in the tooltip) so it can never be read as an official balance.
    const provenance = quotaProvenance(acc);
    const estimatedLimit = Math.max(0, Number(acc.quota_limit || 0));
    const estimated = provenance.confidence === "estimated";
    const confirmedFree = provenance.confidence === "confirmed" && provenance.source === "upstreamExhaustion";
    if (estimatedLimit > 0 && (estimated || confirmedFree)) {
      // Confirmed Free is an upstream-reported balance, never labelled "≈".
      const used = Math.max(0, Math.min(estimatedLimit, Number(acc.quota_used || 0)));
      const remaining = Math.max(0, estimatedLimit - used);
      return {
        supported: true,
        ...(estimated ? { estimated: true } : { confirmedFree: true }),
        limit: estimatedLimit,
        used,
        remaining,
        pctRemaining: Math.max(0, Math.min(100, Math.round((remaining / estimatedLimit) * 100))),
        unit: acc.quota_unit || "tokens",
        windowHours: provenance.windowHours || 24,
        source: provenance.source,
        confidence: provenance.confidence,
        limitKnown: confirmedFree || provenance.limitKnown,
        observed: provenance.observed,
        note: provenance.note,
        resetAt: confirmedFree ? acc.quota_reset_at || "" : "",
      };
    }
    return { supported: false, limit: 0, remaining: 0, used: 0, pctRemaining: 0, quotaUnavailable: true };
  }
  const base = getSidebarQuotaStats(acc);
  if (!base) return null;
  if (base.unknown) return base;
  const limit = Math.max(0, Math.floor(base.limit || 0));
  const remaining = Math.max(0, Math.floor(base.remaining || 0));
  const used = Math.max(0, limit - remaining);
  const pctRemaining = limit > 0 ? Math.min(100, Math.round((remaining / limit) * 100)) : 0;
  return { ...base, limit, remaining, used, pctRemaining };
}

function normalizeAccountSubscription(acc) {
  const raw = String(acc?.subscription || "").trim().toLowerCase();
  if (!raw) return "";
  if (raw.includes("heavy")) return "heavy";
  if (raw.includes("xpremiumplus") || raw.includes("x_premium_plus")) return "x_premium_plus";
  if (raw.includes("xpremium") || raw.includes("x_premium")) return "x_premium";
  if (raw.includes("xbasic") || raw.includes("x_basic")) return "x_basic";
  if (raw.includes("supergrok")) return "supergrok";
  if (raw.includes("super") || raw.includes("pro")) return "super";
  if (raw.includes("lite")) return "lite";
  // "free" must stay its own level: folding it into "basic" made the server's
  // Free verdict render as "basic", which is a different product tier.
  if (raw.includes("free")) return "free";
  if (raw.includes("basic")) return "basic";
  return raw;
}

function subscriptionBadge(acc) {
  const type = normalizeAccountType(acc);
  if (type === "qoder") {
    const plan = String(acc?.quota_plan || "").trim();
    if (!acc?.quota_supported) {
      // No snapshot yet: say so instead of showing a made-up level.
      return {
        text: "未同步",
        bg: "rgba(100, 116, 139, 0.12)",
        color: "#94a3b8",
        tip: "尚未读取到 Qoder 计划与额度；点「检查」立即同步",
      };
    }
    if (plan) {
      const paid = acc?.quota_used !== undefined && plan.toLowerCase().indexOf("free") === -1;
      return {
        text: plan,
        bg: paid ? "rgba(251, 191, 36, 0.16)" : "rgba(100, 116, 139, 0.12)",
        color: paid ? "#fbbf24" : "#cbd5e1",
        tip: `Qoder 计划: ${plan}${acc?.quota_unit ? `（单位 ${acc.quota_unit}）` : ""}`,
      };
    }
    return {
      text: "未知",
      bg: "rgba(100, 116, 139, 0.12)",
      color: "#94a3b8",
      tip: "Qoder 未返回计划档位",
    };
  }
  if (type === "cline") {
    // The tier comes from the upstream plan endpoint, not from the catalog:
    // recommended-models lists four tiers in one payload, so "the free list is
    // non-empty" proves free access and says nothing about a paid plan held
    // alongside it. The server records what /users/me/plan actually answered —
    // a plan name for a subscriber, "free" for an account with no plan history.
    const plan = String(acc?.cline_plan || "").trim();
    if (plan) {
      const free = plan.toLowerCase() === "free";
      return {
        text: free ? "免费" : plan,
        bg: free ? "rgba(52, 211, 153, 0.16)" : "rgba(167, 139, 250, 0.16)",
        color: free ? "#34d399" : "#c4b5fd",
        tip: free
          ? "Cline 免费账号：上游 /users/me/plan 返回没有套餐记录"
          : `Cline 套餐: ${plan}`,
      };
    }
    // No tier recorded yet: an unread plan endpoint is not evidence of free, so
    // the badge says what is missing rather than asserting a tier.
    const modelCount = Array.isArray(acc?.cline_model_ids) ? acc.cline_model_ids.length : 0;
    if (modelCount === 0) {
      return {
        text: "未同步",
        bg: "rgba(100, 116, 139, 0.12)",
        color: "#94a3b8",
        tip: "尚未读取到 Cline 模型目录；点「刷新」立即同步",
      };
    }
    return {
      text: "未同步",
      bg: "rgba(100, 116, 139, 0.12)",
      color: "#94a3b8",
      tip: `已读到 ${modelCount} 个免费模型，但尚未读到套餐档位；点「刷新」重新探测`,
    };
  }
  if (type === "workbuddy") {
    const plan = String(acc?.quota_plan || "").trim();
    if (plan) {
      return {
        text: plan,
        bg: "rgba(52, 211, 153, 0.16)",
        color: "#34d399",
        tip: `WorkBuddy 计量包: ${plan}${acc?.quota_unit ? `（单位 ${acc.quota_unit}）` : ""}`,
      };
    }
    // No meter snapshot yet: say so instead of showing a made-up level.
    return {
      text: "未同步",
      bg: "rgba(100, 116, 139, 0.12)",
      color: "#94a3b8",
      tip: "尚未读取到 WorkBuddy 计量包；点 Sync 刷新账号状态后重试",
    };
  }
  const level = normalizeAccountSubscription(acc);
  if (!level) {
    // Upstream published no plan name. When the quota projection inferred Free, saying
    // "Free（推断）" is more useful than a bare dash — and it stays honest because the
    // badge says 推断 and the tooltip names the signal it came from.
    const provenance = quotaProvenance(acc);
    if (provenance.type === "free" && provenance.confidence) {
      const sourceLabel = QUOTA_SOURCE_LABELS[provenance.source] || provenance.source || "上游数据";
      const confidenceLabel = QUOTA_CONFIDENCE_LABELS[provenance.confidence] || provenance.confidence;
      // "推断" and "已确认" are different claims: the second one is the upstream
      // having refused a request for spending the free window, so it must not be
      // labelled as a guess.
      const confirmed = provenance.confidence === "confirmed";
      return {
        text: confirmed ? "Free（已确认）" : "Free（推断）",
        bg: confirmed ? "rgba(56, 189, 248, 0.16)" : "rgba(148, 163, 184, 0.16)",
        color: confirmed ? "#7dd3fc" : "#cbd5e1",
        tip: `上游未下发套餐名；按「${sourceLabel}」判定为 Free（${confidenceLabel}）`,
      };
    }
    return { text: "-", bg: "rgba(100, 116, 139, 0.12)", color: "#94a3b8", tip: "暂无订阅等级" };
  }
  if (type !== "grok") {
    return { text: level, bg: "rgba(100, 116, 139, 0.12)", color: "#cbd5e1", tip: `订阅等级: ${level}` };
  }
  switch (level) {
    case "unknown":
      return { text: "未知", bg: "rgba(100, 116, 139, 0.12)", color: "#94a3b8", tip: "xAI 未返回可验证的 Grok 套餐等级" };
    case "free":
      // The server only emits this when its own Free inference fired (the plan
      // endpoint reported Free, the billing profile came back empty, or the
      // upstream refused a request for having spent the included free usage).
      return { text: "Free", bg: "rgba(100, 116, 139, 0.12)", color: "#cbd5e1", tip: "Grok Free（上游未下发数值额度；配额列显示估算或实报的 Free 窗口）" };
    case "x_premium_plus":
      return { text: "X Premium+", bg: "rgba(251, 191, 36, 0.16)", color: "#fbbf24", tip: "xAI 官方 X Premium+ 套餐" };
    case "x_premium":
      return { text: "X Premium", bg: "rgba(56, 189, 248, 0.16)", color: "#38bdf8", tip: "xAI 官方 X Premium 套餐" };
    case "x_basic":
      return { text: "X Basic", bg: "rgba(167, 139, 250, 0.16)", color: "#c4b5fd", tip: "xAI 官方 X Basic 套餐" };
    case "supergrok":
      return { text: "SuperGrok", bg: "rgba(251, 191, 36, 0.16)", color: "#fbbf24", tip: "xAI 官方 SuperGrok 套餐" };
    case "heavy":
      return { text: "heavy", bg: "rgba(251, 191, 36, 0.16)", color: "#fbbf24", tip: "Grok Heavy 账号池" };
    case "super":
      return { text: "super", bg: "rgba(56, 189, 248, 0.16)", color: "#38bdf8", tip: "Grok Super 账号池" };
    case "lite":
      return { text: "lite", bg: "rgba(167, 139, 250, 0.16)", color: "#c4b5fd", tip: "Grok Lite 账号池" };
    case "basic":
      return { text: "basic", bg: "rgba(52, 211, 153, 0.14)", color: "#34d399", tip: "Grok Basic 账号池" };
    default:
      return { text: level, bg: "rgba(100, 116, 139, 0.12)", color: "#cbd5e1", tip: `Grok 账号池: ${level}` };
  }
}

function buildSubscriptionMarkup(acc) {
  const badge = subscriptionBadge(acc);
  return `<span class="tag account-tier-tag" title="${escapeHtml(badge.tip || "")}" style="background:${badge.bg};color:${badge.color};border:none;">${escapeHtml(badge.text)}</span>`;
}

function applyTokenLabels(type) {
  const normalized = String(type || "").trim().toLowerCase();
  const accountId = String(document.getElementById("accountId")?.value || "");
  // WorkBuddy login stays available while editing so an expired authorization can
  // be renewed by signing in again instead of deleting the account.
  const workbuddyLoginGroup = document.getElementById("workbuddyLoginGroup");
  if (workbuddyLoginGroup) workbuddyLoginGroup.hidden = normalized !== "workbuddy";
  // Qoder is OAuth-only as well: an existing account can be re-authorized
  // through the same browser flow, so the group stays visible while editing.
  const qoderLoginGroup = document.getElementById("qoderLoginGroup");
  if (qoderLoginGroup) qoderLoginGroup.hidden = normalized !== "qoder";
  // Cline is OAuth-only for the same reason: the WorkOS device grant is the only
  // way to obtain the credential, and an existing account is renewed by signing
  // in again.
  const clineLoginGroup = document.getElementById("clineLoginGroup");
  if (clineLoginGroup) clineLoginGroup.hidden = normalized !== "cline";
  const saveButton = document.querySelector('#accountForm button[type="submit"]');
  if (saveButton) {
    // WorkBuddy, Qoder, Cline and Grok are created by their official login
    // flows, so the form has nothing to submit for a new account of any of
    // those types.
    const loginOnlyChannel =
      normalized === "workbuddy" || normalized === "qoder" || normalized === "cline" || normalized === "grok";
    saveButton.hidden = loginOnlyChannel && !accountId;
  }
  applyCredentialModeUI(normalized);
}

// Grok exposes a server-owned device-auth protocol. The lifecycle is shared
// with the other official-login channels while provider endpoints, copy and
// URL allowlists remain explicit, so changing one provider cannot silently
// widen another.
function createAccountDeviceLogin(options) {
  const state = { id: "", timer: null };
  const statusNode = () => document.getElementById(options.statusId);

  function render(message, type = "info", html = "") {
    const node = statusNode();
    if (!node) return;
    node.hidden = false;
    node.classList.toggle("is-active", type === "info");
    node.classList.toggle("is-error", type === "error");
    node.innerHTML = `<strong>${escapeImportStatusText(message)}</strong>${html}`;
  }

  function reset() {
    const node = statusNode();
    if (!node) return;
    node.hidden = true;
    node.classList.remove("is-active", "is-error");
    node.innerHTML = "";
  }

  function stop(cancel = false) {
    if (state.timer) {
      clearTimeout(state.timer);
      state.timer = null;
    }
    const id = state.id;
    state.id = "";
    const button = document.getElementById(options.buttonId);
    if (button) button.disabled = false;
    if (cancel && id) {
      fetch(`${options.endpoint}/${encodeURIComponent(id)}`, { method: "DELETE" }).catch(() => {});
    }
  }

  async function poll() {
    const id = state.id;
    if (!id) return;
    try {
      const res = await fetch(`${options.endpoint}/${encodeURIComponent(id)}`);
      if (!res.ok) throw new Error(await res.text());
      const login = await res.json();
      if (id !== state.id) return;
      if (login.status === "pending") {
        state.timer = setTimeout(poll, 1500);
        return;
      }
      stop(false);
      if (login.status === "complete") {
        const message = login.message || `${options.provider} 账号已添加`;
        render(message, "info");
        showToast(message, "success");
        loadAccounts();
        setTimeout(closeModal, 800);
        return;
      }
      render(login.message || `${options.provider} 官方登录未完成`, "error");
    } catch (err) {
      if (id !== state.id) return;
      stop(false);
      render(`${options.provider} 登录状态查询失败：` + (err.message || String(err)), "error");
    }
  }

  async function start() {
    if (state.id) return;
    const button = document.getElementById(options.buttonId);
    if (button) button.disabled = true;
    reset();
    try {
      const res = await fetch(options.endpoint, { method: "POST" });
      if (!res.ok) throw new Error(await res.text());
      const login = await res.json();
      state.id = String(login.id || "");
      if (!state.id || login.status !== "pending") {
        throw new Error(`${options.provider} 登录初始化响应无效`);
      }
      const link = String(login.verification_uri_complete || login.verification_uri || "");
      let safeLink = "";
      try {
        const parsed = new URL(link);
        if (options.isAllowedURL(parsed)) safeLink = parsed.href;
      } catch (_) {
        // An unexpected upstream URL must never become an open redirect.
      }
      const linkHTML = safeLink
        ? `<div style="margin-top:8px"><a href="${escapeImportStatusText(safeLink)}" target="_blank" rel="noopener noreferrer">打开 ${options.provider} 官方授权页面</a></div>`
        : "";
      render(`请在 ${options.provider} 官方页面输入设备码：${login.user_code || ""}`, "info", linkHTML);
      if (safeLink) window.open(safeLink, "_blank", "noopener");
      poll();
    } catch (err) {
      stop(false);
      render(`无法启动 ${options.provider} 官方登录：` + (err.message || String(err)), "error");
    }
  }

  return { render, reset, stop, start, poll };
}

const grokDeviceLogin = createAccountDeviceLogin({
  provider: "Grok",
  endpoint: "/api/grok/device-auth",
  statusId: "grokDeviceLoginStatus",
  buttonId: "grokDeviceLoginButton",
  isAllowedURL: (url) => url.protocol === "https:" && (url.hostname === "auth.x.ai" || url.hostname === "accounts.x.ai"),
});

function resetGrokDeviceLoginStatus() { grokDeviceLogin.reset(); }
function stopGrokDeviceLogin(cancel = false) { grokDeviceLogin.stop(cancel); }
function startGrokDeviceLogin() { return grokDeviceLogin.start(); }

function applyCredentialModeUI(type) {
  const grokDeviceLoginGroup = document.getElementById("grokDeviceLoginGroup");
  if (grokDeviceLoginGroup) grokDeviceLoginGroup.hidden = String(type || "").trim().toLowerCase() !== "grok";
}

function escapeImportStatusText(text) {
  return String(text || "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
}

function accountTypeLabel(type) {
  return window.OrchidsProviderRegistry?.label(type) || String(type || "").trim();
}

function getActiveAccountType() {
  const platform = String(currentPlatform || "").trim().toLowerCase();
  return platform || window.OrchidsProviderRegistry?.defaultProviderKey || "";
}

// selectedPlatformAccountType resolves the account type for a brand-new account.
//
// The active platform tab is the source of truth: it is what the operator is
// looking at when they press 添加账号. The hidden field is only a fallback for the
// cold-load case (no tab rendered yet) — reading it first let a stale value from
// a previous modal interaction silently win, which made the Grok tab open
// another channel's form.
function selectedPlatformAccountType(typeEl) {
  const activeTab = document.querySelector("#platformFilters .tab-item.active");
  if (activeTab?.dataset?.platform) {
    const visible = decodeURIComponent(activeTab.dataset.platform);
    if (visible) return platformAccountType(visible);
  }
  const active = String(currentPlatform || "").trim().toLowerCase();
  if (active) return platformAccountType(active);
  const selected = String(typeEl?.value || "").trim().toLowerCase();
  return selected || getActiveAccountType();
}

// platformAccountType maps a platform tab key to the account type it creates.
// Tab keys are account types (the tab list is built from `accounts[].account_type`),
// so an unrecognised value must not silently create an account of the wrong
// channel.
function platformAccountType(platform) {
  const key = String(platform || "").trim().toLowerCase();
  return window.OrchidsProviderRegistry?.get(key)?.key || getActiveAccountType();
}

function setAccountModalType(type) {
  const fallback = window.OrchidsProviderRegistry?.defaultProviderKey || "";
  const normalized = String(type || fallback).trim().toLowerCase() || fallback;
  const typeEl = document.getElementById("accountType");
  const displayEl = document.getElementById("accountTypeDisplay");
  if (typeEl) typeEl.value = normalized;
  if (displayEl) displayEl.value = accountTypeLabel(normalized);
  applyTokenLabels(normalized);
}

// extractAdminErrorDetail keeps update failures readable without exposing raw JSON.
function extractAdminErrorDetail(raw) {
  const text = String(raw == null ? "" : raw).trim();
  if (!text) return "";
  try {
    const parsed = JSON.parse(text);
    const message = parsed && typeof parsed === "object"
      ? (parsed.error && typeof parsed.error === "object" ? parsed.error.message : parsed.error)
      : null;
    const detail = typeof message === "string" ? message.trim() : "";
    if (detail) return detail;
  } catch (_) {
    /* a plain-text error body is already the detail */
  }
  return text;
}

// The console's channel strip, shared with 模型管理: the same channels, in the same
// order, with the same names, so an operator moving between the two pages does not have
// the strip reorder under them.
//
// Cline is listed here because this list is the only thing that renders a channel
// tab: a channel missing from it has no tab, so its login group in the account
// modal can never be selected and the channel is unreachable from the console.
const ACCOUNT_PLATFORM_ORDER = window.OrchidsProviderRegistry?.keys || [];
const ACCOUNT_TYPE_NAMES = Object.fromEntries(
  (window.OrchidsProviderRegistry?.providers || []).map((item) => [item.key, item.label]),
);

// One name for the selected channel, used by the strip, the subtitle, the toasts and the
// empty state. currentPlatform stays the lower-case key the API stores; nothing shows it.
// clinePageOnly reports whether the page is showing the Cline channel alone.
//
// The console renders one platform at a time; the unfiltered view mixes every
// channel, and a column that is noise for one of them is still the only place
// another reports its balance. So the decision is made on the filter, never on
// the row: mixed rows keep every column.
function clinePageOnly() {
  return String(currentPlatform || "").trim().toLowerCase() === "cline";
}

function currentPlatformLabel() {
  const key = String(currentPlatform || "").trim();
  if (!key) return "";
  return ACCOUNT_TYPE_NAMES[key.toLowerCase()] || key;
}

function orderedAccountPlatforms() {  const ordered = ACCOUNT_PLATFORM_ORDER.slice();
  // A type the console has not been taught about is appended, not dropped.
  accounts
    .map(normalizeAccountType)
    .filter(Boolean)
    .filter((type) => !ordered.includes(type))
    .sort()
    .forEach((type) => {
      if (!ordered.includes(type)) ordered.push(type);
    });
  return ordered;
}

// Render platform filter tabs
function renderPlatformTabs() {
  const container = document.getElementById("platformFilters");
  if (!container) return;
  const tabs = orderedAccountPlatforms();

  if (currentPlatform === '' || !tabs.includes(currentPlatform)) {
    // The first channel of the shared order, exactly as the models page picks its own.
    currentPlatform = tabs.length > 0 ? tabs[0] : '';
  }

  container.innerHTML = "";
  tabs.forEach(type => {
    const label = String(type || "");
    const isActive = currentPlatform === label;
    const btn = document.createElement("button");
    btn.className = `tab-item ${isActive ? 'active' : ''}`.trim();
    btn.dataset.platform = encodeURIComponent(label);
    // The key stays in dataset (it is what the filter matches); the strip shows the name.
    btn.textContent = ACCOUNT_TYPE_NAMES[label.toLowerCase()] || label;
    btn.addEventListener("click", () => {
      const raw = btn.dataset.platform ? decodeURIComponent(btn.dataset.platform) : "";
      filterByPlatform(raw);
    });
    container.appendChild(btn);
  });
}

// Update account health status
function updateAccountHealth(id, ok, msg = '') {
  accountHealth[id] = {
    ok,
    msg,
    checkedAt: new Date().toISOString(),
  };
}

function evaluateAccountStatus(acc) {
  const health = accountHealth[acc.id];
  if (health && !health.ok) {
    return { normal: false, text: '异常', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: health.msg || '状态同步失败' };
  }
  if (!acc.enabled) {
    return { normal: false, text: '禁用', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '账号已禁用' };
  }
  const statusCode = normalizeSidebarStatusCode(acc.status_code);
  // A bare "401" or "429" hides the actionable cause (retired grant vs. a
  // partial write vs. upstream throttling), so the server's reason wins.
  const statusReason = String(acc.status_message || "").trim();
  if (isQuotaOnlyStatus(acc)) {
    const quota = getQuotaStats(acc);
    const limitText = quota && quota.limit > 0 ? quota.limit.toLocaleString() : '未知';
    const type = normalizeAccountType(acc);
    const providerName = accountTypeLabel(type);
    return {
      normal: true,
      text: '额度不足',
      color: '#f59e0b',
      bg: 'rgba(245, 158, 11, 0.16)',
      tip: providerName + ' 额度已用尽或余额不足，调度器会暂时跳过该账号 (剩余 0 / ' + limitText + ')',
      quotaOnly: true,
    };
  }
  if (statusCode) {
    // When the server recorded a reason, it is the actionable text; the generic
    // per-code wording is only the fallback.
    const withReason = (fallback) => statusReason || fallback;
    switch (statusCode) {
      case '429':
        return { normal: false, text: '限流', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: withReason('请求过于频繁 (429)') };
      case '401':
        return { normal: false, text: '未授权', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: withReason('认证失败 (401)') };
      case '403':
        return { normal: false, text: '禁止访问', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: withReason('访问被拒绝 (403)') };
      case '404':
        return { normal: false, text: '不存在', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: withReason('资源不存在 (404)') };
      default:
        return { normal: false, text: '异常', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: withReason('状态异常: ' + statusCode) };
    }
  }

  if (!hasSidebarAccountCredential(acc)) {
    const type = normalizeAccountType(acc);
    const tips = {
      grok: '缺少 Build OAuth 授权',
      workbuddy: '缺少 WorkBuddy 凭证（refreshToken / accessToken）',
      qoder: '缺少 Qoder 设备凭据，请重新使用官方网页登录',
      cline: '缺少 Cline WorkOS 凭据，请重新使用官方网页登录',
    };
    return {
      normal: false,
      text: type === 'grok' ? '待登录' : '待补全',
      color: '#f59e0b',
      bg: 'rgba(245, 158, 11, 0.16)',
      tip: tips[type] || '缺少账号凭据',
    };
  }

  const quota = getQuotaStats(acc);
  // A drained allowance is the same business limit on every channel: the
  // credential is intact, the row stays selectable, and the scheduler resumes it
  // when the window resets. An exhausted Grok window used to show a red 配额已满
  // fault and disagree with the sidebar counter.
  if (isQuotaExhaustedQuota(acc, quota)) {
    const providerName = accountTypeLabel(normalizeAccountType(acc));
    const limitText = quota && quota.limit > 0 ? quota.limit.toLocaleString() : '未知';
    return {
      normal: true,
      text: '额度不足',
      color: '#f59e0b',
      bg: 'rgba(245, 158, 11, 0.16)',
      tip: providerName + ' 额度已用尽或余额不足，调度器会暂时跳过该账号 (剩余 0 / ' + limitText + ')',
      quotaOnly: true,
    };
  }

  return { normal: true, text: '正常', color: '#34d399', bg: 'rgba(52, 211, 153, 0.16)', tip: '状态正常' };
}

function isAccountAbnormal(acc) {
  return !evaluateAccountStatus(acc).normal;
}

function matchesCurrentPlatform(acc) {
  if (!currentPlatform) return true;
  const key = String(currentPlatform || "").toLowerCase();
  return normalizeAccountType(acc).includes(key);
}

// Get status badge for account
function statusBadge(acc) {
  return evaluateAccountStatus(acc);
}

// Refresh single account via the shared check endpoint. Returns true when the
// sync succeeded, so callers (auto-sync in particular) can tell a refreshed row
// from a failed attempt.
async function checkAccount(id, silent = false, actionText = "刷新") {
  const action = "check";
  let succeeded = false;
  try {
    const res = await fetch(`/api/accounts/${id}/${action}`);
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const updated = await res.json();
    accounts = accounts.map(a => (a.id === id ? updated : a));
    updateAccountHealth(id, true);
    succeeded = true;
    if (!silent) showToast(`账号 ${updated.name || updated.email || id} ${actionText}完成`, "success");
  } catch (err) {
    try {
      const latestRes = await fetch(`/api/accounts/${id}`);
      if (latestRes.ok) {
        const latest = await latestRes.json();
        accounts = accounts.map(a => (a.id === id ? latest : a));
        delete accountHealth[id];
      } else {
        updateAccountHealth(id, false, err.message || String(err));
      }
    } catch (_) {
      updateAccountHealth(id, false, err.message || String(err));
    }
    if (!silent) showToast(`账号 ${id} ${actionText}失败`, "error");
  } finally {
    renderAccounts();
    updateStats();
  }
  return succeeded;
}

// Clear abnormal accounts
async function clearAbnormalAccounts() {
  const abnormal = accounts.filter((acc) => matchesCurrentPlatform(acc) && isAccountAbnormal(acc));
  if (abnormal.length === 0) {
    showToast(currentPlatform ? `当前 ${currentPlatformLabel()} 页面没有异常账号` : "没有异常账号", "info");
    return;
  }
  const scopeText = currentPlatform ? `当前 ${currentPlatformLabel()} 页面中的 ` : "";
  if (confirm(`确定要清空 ${scopeText}${abnormal.length} 个异常账号吗？`)) {
    for (const acc of abnormal) {
      await fetch(`/api/accounts/${acc.id}`, { method: "DELETE" });
    }
    loadAccounts();
    showToast(`已清空${scopeText}异常账号`);
  }
}

// Batch delete accounts
async function batchDeleteAccounts() {
  const selected = Array.from(document.querySelectorAll(".row-checkbox:checked")).map(cb => cb.dataset.id);
  if (selected.length === 0) return;
  if (confirm(`确定要删除选中的 ${selected.length} 个账号吗？`)) {
    for (const id of selected) {
      await fetch(`/api/accounts/${id}`, { method: "DELETE" });
    }
    loadAccounts();
    showToast(`已成功删除 ${selected.length} 个账号`);
  }
}

// Render accounts table
function renderAccounts() {
  const container = domCache.accountsList || document.getElementById("accountsList");
  const filtered = accounts.filter(matchesCurrentPlatform);

  const total = filtered.length;
  const totalPages = Math.ceil(total / pageSize) || 1;
  if (currentPage > totalPages) currentPage = totalPages;
  if (currentPage < 1) currentPage = 1;

  const start = (currentPage - 1) * pageSize;
  const end = start + pageSize;
  const pageItems = filtered.slice(start, end);

  if (pageItems.length === 0) {
    container.innerHTML = "";
    const empty = document.createElement("div");
    empty.className = "empty-state empty-state-panel";
    const icon = document.createElement("span");
    icon.className = "empty-state-mark";
    icon.textContent = "EMPTY";
    const text = document.createElement("p");
    text.textContent = currentPlatformLabel() ? `暂无 ${currentPlatformLabel()} 账号数据` : "暂无账号数据";
    empty.appendChild(icon);
    empty.appendChild(text);
    container.appendChild(empty);
    const paginationInfo = domCache.paginationInfo || document.getElementById("paginationInfo");
    paginationInfo.textContent = `共 0 条记录，第 1/1 页`;
    renderPagination(1, 1);
    return;
  }

  if (window.matchMedia("(max-width: 640px)").matches) {
    renderAccountsMobile(container, pageItems, total, totalPages);
    return;
  }

  container.innerHTML = "";
  const wrap = document.createElement("div");
  wrap.className = "table-wrap";
  const table = document.createElement("table");
  table.className = "accounts-table";
  const thead = document.createElement("thead");
  const headRow = document.createElement("tr");
  // Cline has no numeric allowance, so its 配额 cell could only ever read
  // "未计量" — a column of noise. It is dropped on the Cline page only: the
  // other five channels report a real balance and keep theirs. The whole page
  // renders one channel at a time, so one filter decision covers every row.
  const quotaColumnVisible = !clinePageOnly();
  const headers = [
    { label: "", className: "col-check" },
    { label: "ID", className: "col-id" },
    { label: "账号" },
    { label: "等级", className: "col-tier" },
    ...(quotaColumnVisible ? [{ label: "配额", className: "col-quota" }] : []),
    { label: "状态", className: "col-status" },
    // 今日/累计 Tokens is the only spend figure an unmetered channel can
    // offer, and the only one that answers "how close is this account to the
    // upstream rate limit right now".
    { label: "今日/累计 Tokens", className: "col-tokens", title: "本网关本地统计，不代表官方额度" },
    { label: "调用", className: "col-usage" },
    { label: "创建时间", className: "col-created" },
    { label: "操作", className: "col-actions" },
  ];
  headers.forEach((h, idx) => {
    const th = document.createElement("th");
    if (h.className) th.className = h.className;
    if (h.title) th.title = h.title;
    if (idx === 0) {
      const selectAll = document.createElement("input");
      selectAll.type = "checkbox";
      selectAll.dataset.action = "select-all";
      th.appendChild(selectAll);
    } else {
      th.textContent = h.label;
    }
    headRow.appendChild(th);
  });
  thead.appendChild(headRow);
  table.appendChild(thead);

  const tbody = document.createElement("tbody");

  // 使用 DocumentFragment 批量构建表格行
  const fragment = document.createDocumentFragment();
  pageItems.forEach((acc) => {
    const badge = statusBadge(acc);
    const tokenDisplay = formatTokenDisplay(acc);
    const identity = accountIdentityPrimary(acc);
    const tr = document.createElement("tr");

    const tdCheck = document.createElement("td");
    tdCheck.className = "col-check";
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.className = "row-checkbox";
    cb.dataset.action = "row-select";
    cb.dataset.id = encodeData(acc.id);
    tdCheck.appendChild(cb);
    tr.appendChild(tdCheck);

    const tdID = document.createElement("td");
    tdID.className = "col-id";
    tdID.textContent = acc.id === null || acc.id === undefined ? "" : String(acc.id);
    tr.appendChild(tdID);

    // The identity cell leads with the address/identifier and keeps the
    // credential summary as secondary text: a truncated token used to be the
    // most prominent thing in the row.
    const tdToken = document.createElement("td");
    tdToken.className = "col-token";
    const identityWrap = document.createElement("div");
    identityWrap.className = "account-identity";
    const primary = document.createElement("span");
    primary.className = "account-identity-primary";
    primary.textContent = identity;
    primary.title = identity;
    identityWrap.appendChild(primary);
    const sub = document.createElement("span");
    sub.className = "account-identity-sub";
    const typeBadge = document.createElement("span");
    typeBadge.className = "badge badge-" + normalizeAccountType(acc);
    // The models table and account table use the same display names.
    typeBadge.textContent = ACCOUNT_TYPE_NAMES[normalizeAccountType(acc)] || normalizeAccountType(acc) || "unknown";
    sub.appendChild(typeBadge);
    if (tokenDisplay && tokenDisplay !== identity && tokenDisplay !== "-") {
      const credential = document.createElement("span");
      credential.className = "account-identity-token";
      credential.textContent = tokenDisplay;
      credential.title = tokenDisplay;
      sub.appendChild(credential);
    }
    identityWrap.appendChild(sub);
    tdToken.appendChild(identityWrap);
    tr.appendChild(tdToken);

    const tdTier = document.createElement("td");
    tdTier.className = "col-tier";
    tdTier.innerHTML = buildSubscriptionMarkup(acc);
    tr.appendChild(tdTier);

    // Dropped on the Cline page for the same reason the header is: a column that
    // can only ever say "未计量" is not worth a column.
    if (quotaColumnVisible) {
      const tdQuota = document.createElement("td");
      tdQuota.className = "col-quota";
      // One shared renderer for the desktop table and the mobile cards.
      tdQuota.innerHTML = buildQuotaMarkup(acc);
      const quota = getQuotaStats(acc);
      if (normalizeAccountType(acc) === "workbuddy") {
        if (quota && quota.workbuddy) {
          tdQuota.title = [
            quota.plan ? `计量包: ${quota.plan}` : "",
            `单位: ${quota.unit || "credit"}`,
            "口径: 当前周期剩余 / 周期上限",
            quota.resetAt ? `重置: ${new Date(quota.resetAt).toLocaleString()}` : "",
          ].filter(Boolean).join(" · ");
        } else {
          tdQuota.title = "尚未读取到 WorkBuddy 计量额度；点刷新立即同步";
        }
      } else if (normalizeAccountType(acc) === "qoder") {
        if (quota && quota.supported) {
          tdQuota.title = [
            quota.plan ? `计划: ${quota.plan}` : "",
            `单位: ${quota.unit || "credits"}`,
            "口径: 当前窗口剩余 / 窗口额度",
            quota.exhausted ? "该账号额度已用尽，窗口重置后自动恢复" : "",
            quota.resetAt ? `重置: ${new Date(quota.resetAt).toLocaleString()}` : "",
            quota.upgradeUrl ? `升级: ${quota.upgradeUrl}` : "",
          ].filter(Boolean).join(" · ");
        } else {
          tdQuota.title = "尚未读取到 Qoder 计划与额度；点「检查」立即同步";
        }
      } else if (quota && (quota.estimated || quota.quotaUnavailable)) {
        // Every number in this cell carries its provenance, so an estimate is never
        // mistaken for a reported balance.
        tdQuota.title = quotaTooltip(acc, quota);
      }
      tr.appendChild(tdQuota);
    }

    const tdStatus = document.createElement("td");
    tdStatus.className = "col-status";
    const statusSpan = document.createElement("span");
    statusSpan.className = "tag tag-status-normal";
    statusSpan.title = badge.tip || "";
    statusSpan.style.background = badge.bg;
    statusSpan.style.color = badge.color;
    statusSpan.style.border = "none";
    statusSpan.textContent = badge.text;
    tdStatus.appendChild(statusSpan);
    // A cooled account without a stated recovery time reads as broken forever.
    // The scheduler always writes a deadline when it cools one, so the line
    // appears exactly when the account is held and says when to come back.
    const cooldown = document.createElement("div");
    cooldown.innerHTML = buildCooldownMarkup(acc);
    if (cooldown.innerHTML) tdStatus.appendChild(cooldown);
    tr.appendChild(tdStatus);

    const tdTokens = document.createElement("td");
    tdTokens.className = "col-tokens";
    tdTokens.innerHTML = buildTokensMarkup(acc);
    tr.appendChild(tdTokens);

    // One usage cell: the count carries the meaning, the last-use time is a
    // sub-line instead of a column of its own.
    const tdUsage = document.createElement("td");
    tdUsage.className = "col-usage";
    const count = document.createElement("div");
    count.className = "account-usage-count";
    count.textContent = String(accountUsageCounter(acc));
    if (normalizeAccountType(acc) === "workbuddy") {
      count.title = "WorkBuddy 按计量口径统计的已消耗额度（点刷新同步）";
    }
    const when = document.createElement("div");
    when.className = "account-usage-when";
    when.textContent = acc.last_used_at && !acc.last_used_at.startsWith('0001') ? formatTime(acc.last_used_at) : "未调用";
    tdUsage.appendChild(count);
    tdUsage.appendChild(when);
    tr.appendChild(tdUsage);

    const tdCreated = document.createElement("td");
    tdCreated.className = "col-created";
    tdCreated.innerHTML = buildCreatedMarkup(acc);
    tr.appendChild(tdCreated);

    const tdActions = document.createElement("td");
    tdActions.className = "col-actions";
    const actionWrap = document.createElement("div");
    actionWrap.className = "accounts-row-actions";

    const edit = document.createElement("button");
    edit.type = "button";
    edit.className = "action-icon";
    edit.dataset.action = "edit";
    edit.dataset.id = encodeData(acc.id);
    edit.title = "编辑账号设置与凭据";
    edit.textContent = "编辑";

    const refresh = document.createElement("button");
    refresh.type = "button";
    refresh.className = "action-icon";
    refresh.dataset.action = "refresh";
    refresh.dataset.id = encodeData(acc.id);
    refresh.title = "立即同步状态与额度";
    refresh.textContent = "刷新";

    const del = document.createElement("button");
    del.type = "button";
    del.className = "action-icon is-danger";
    del.dataset.action = "delete";
    del.dataset.id = encodeData(acc.id);
    del.title = "删除该账号";
    del.textContent = "删除";

    actionWrap.appendChild(edit);
    actionWrap.appendChild(refresh);
    actionWrap.appendChild(del);
    tdActions.appendChild(actionWrap);
    tr.appendChild(tdActions);

    // 将行添加到 fragment 而不是直接添加到 tbody
    fragment.appendChild(tr);
  });

  // 一次性将所有行插入到 tbody
  tbody.appendChild(fragment);
  table.appendChild(tbody);
  wrap.appendChild(table);
  container.appendChild(wrap);

  const paginationInfo = domCache.paginationInfo || document.getElementById("paginationInfo");
  paginationInfo.textContent = `共 ${total} 条记录，第 ${currentPage}/${totalPages} 页`;
  renderPagination(currentPage, totalPages);
  updateSelectedCount();

  bindAccountActions(container);
}

// formatCredit renders meter values that the upstream reports with fractions.
function formatCredit(value) {
  const number = Number(value || 0);
  if (!Number.isFinite(number)) return "0";
  return Number.isInteger(number) ? number.toLocaleString() : number.toFixed(2);
}

// formatQuotaReset renders a reset timestamp as a compact remaining time.
function formatQuotaReset(iso) {
  const resetAt = Date.parse(String(iso || ""));
  if (!Number.isFinite(resetAt)) return "";
  const remaining = resetAt - Date.now();
  if (remaining <= 0) return "待重置";
  const hours = Math.floor(remaining / 3600000);
  if (hours < 24) return `${Math.max(1, hours)} 小时后重置`;
  return `${Math.floor(hours / 24)} 天后重置`;
}

// formatTokenCount abbreviates a token count the way a usage column has to: the
// interesting values are five and six digits long, and "23849" next to a model
// name costs more attention than "23.8K".
function formatTokenCount(value) {
  const n = Number(value || 0);
  if (!Number.isFinite(n) || n <= 0) return "0";
  if (n >= 1000000) return (n / 1000000).toFixed(2).replace(/\.?0+$/, "") + "M";
  if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, "") + "K";
  return String(Math.round(n));
}

// accountTokensToday is the spend counted for the account's current local day.
//
// The counter stamps the day it counted, so a figure whose date is not today is
// yesterday's: it must read as 0 rather than as a stale number. An account that
// predates the counter has no date at all, which is "not measured yet" — the
// lifetime total is still the honest answer, so it is reported as-is.
function accountTokensToday(acc) {
  const value = Number(acc?.tokens_today || 0);
  if (!Number.isFinite(value) || value <= 0) return 0;
  const stamp = String(acc?.tokens_date || "").trim();
  if (!stamp) return 0;
  return stamp === gatewayDayStamp() ? value : 0;
}

// The gateway rolls tokens_today using the server day. us1 runs in UTC, while
// operators commonly open the UI in Asia/Shanghai; comparing that server stamp
// with the browser's local date made every account read 0 for eight hours after
// Shanghai midnight. UTC is deployment-independent and matches the persisted
// counter contract.
function gatewayDayStamp(date) {
  // Dates passed from tests/plugins can come from another JS realm, where
  // `instanceof Date` is false. Accept every real Date by value instead of
  // silently replacing it with the current time.
  const candidate = date != null ? new Date(date.valueOf()) : new Date();
  const d = Number.isNaN(candidate.getTime()) ? new Date() : candidate;
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`;
}

// buildTokensMarkup renders the 今日/累计 Tokens cell.
//
// A lifetime total alone cannot answer how much of the upstream rate limit the
// account has spent right now, because it only ever grows — which is the one
// number an unmetered channel has to offer. Both figures are local: the gateway
// counts what it saw, and the tooltip says so instead of implying the upstream
// reported them.
function buildTokensMarkup(acc) {
  const today = accountTokensToday(acc);
  const total = Number(acc?.usage_total || 0);
  const title = `今日 ${Math.round(today).toLocaleString()} / 累计 ${Math.round(total).toLocaleString()} tokens（本网关本地统计：上游返回 usage 时精确，否则按请求体估算）`;
  return `<span class="account-tokens" title="${escapeHtml(title)}">${formatTokenCount(today)} / ${formatTokenCount(total)}</span>`;
}

// cooldownRecoveryAt is the instant the account is expected to serve again.
//
// The scheduler writes one deadline per cause; whichever is furthest out is the
// one that still holds the account, so that is the one worth printing.
function cooldownRecoveryAt(acc) {
  const candidates = [acc?.quota_reset_at, acc?.quality_cooldown_until];
  let latest = 0;
  for (const raw of candidates) {
    const at = Date.parse(String(raw || ""));
    if (Number.isFinite(at) && at > latest) latest = at;
  }
  return latest > 0 ? latest : 0;
}

// buildCooldownMarkup renders the recovery line under the status badge.
//
// A cooled account with no stated recovery time looks broken forever. The
// scheduler always sets a deadline when it cools one, so the absence of a line
// means "not cooled" — and its presence should say when to come back.
function buildCooldownMarkup(acc) {
  const badge = evaluateAccountStatus(acc);
  if (badge.normal) return "";
  const at = cooldownRecoveryAt(acc);
  if (!at) return "";
  const remaining = at - Date.now();
  const when = new Date(at).toLocaleString("zh-CN", {
    month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false,
  });
  const tail = remaining > 0 ? `（${formatRemainingCompact(remaining)}）` : "（已到期，等待下一次调度）";
  return `<div class="account-cooldown-until" style="font-size:0.68rem;color:#64748b;margin-top:2px">预计 ${escapeHtml(when)} 恢复${escapeHtml(tail)}</div>`;
}

// formatRemainingCompact renders a cooldown span as hours/minutes.
//
// Rounding happens once, at the finest unit, and the coarser units derive from
// it: flooring the hours directly turned a 2h59m59s wait into "2 小时后" while
// the clock above it already read 12:00.
function formatRemainingCompact(ms) {
  const minutes = Math.round(ms / 60000);
  if (minutes < 60) return `${Math.max(1, minutes)} 分钟后`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时后`;
  return `${Math.floor(hours / 24)} 天后`;
}

// buildCreatedMarkup renders the account creation time.
//
// It is the only column that lets an operator tell a freshly added account from
// one that has been rotated several times, and it is what makes an aged account
// with a low request count read as "idle" rather than "broken".
function buildCreatedMarkup(acc) {
  const createdAt = Date.parse(String(acc?.created_at || ""));
  if (!Number.isFinite(createdAt) || createdAt <= 0) return `<span style="color:#64748b">-</span>`;
  return `<span class="account-created" style="font-size:0.74rem">${escapeHtml(formatDate(new Date(createdAt)))}</span>`;
}

// formatDate renders a calendar date without the relative-time shortcut: an
// absolute date is what you compare against another account's date.
function formatDate(d) {
  if (!(d instanceof Date) || Number.isNaN(d.getTime())) return "-";
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

// accountUsageCounter is the value the 调用 column shows. WorkBuddy accounts have
// no request counter of their own, so the credit meter's consumed units are the
// honest equivalent.
function accountUsageCounter(acc) {
  const type = normalizeAccountType(acc);
  if (type === "workbuddy") {
    const consumed = Number(acc?.quota_consumed_units);
    if (Number.isFinite(consumed) && consumed > 0) return consumed;
  }
  return Number(acc?.request_count || 0);
}

// buildMobileEmailMarkup surfaces the signed-in address, which is how operators
// actually recognise a WorkBuddy account (the nickname is the email).
function buildMobileEmailMarkup(acc) {
  const identity = normalizeAccountType(acc) === "workbuddy" ? workBuddyIdentityLabel(acc) : String(acc?.email || "").trim();
  if (!identity || identity === "-") return "";
  const label = normalizeAccountType(acc) === "workbuddy" ? "账号 / 邮箱" : "邮箱";
  return `
        <div class="account-mobile-item" style="grid-column: 1 / -1;">
          <span class="account-mobile-label">${label}</span>
          <span class="account-mobile-value" style="word-break: break-all;">${escapeHtml(identity)}</span>
        </div>`;
}

// accountIdentityPrimary is the row's subject: the address for channels that
// report one (WorkBuddy's nickname is the email), otherwise the credential
// summary.
function accountIdentityPrimary(acc) {
  const type = normalizeAccountType(acc);
  if (type === "workbuddy") {
    const label = workBuddyIdentityLabel(acc);
    if (label && label !== "-") return label;
  }
  const email = String(acc?.email || "").trim();
  const name = String(acc?.name || "").trim();
  const display = formatTokenDisplay(acc);
  const identity = email || name || (display && display !== "-" ? display : "未命名账号");
  if (type === "grok") return `${identity} · Build OAuth`;
  return identity;
}

// buildQuotaMarkup renders the remaining allowance for every channel.
function buildQuotaMarkup(acc) {
  const quota = getQuotaStats(acc);
  if (quota && quota.confirmedFree) {
    // A real Free window reported by the upstream is a balance, not an estimate, so it
    // is rendered without "≈" while still naming what kind of window it is.
    const pct = quota.pctRemaining;
    const color = pct <= 10 ? "#fb7185" : pct <= 30 ? "#f59e0b" : "#34d399";
    const windowText = quota.windowHours ? `滚动 ${quota.windowHours}h` : "";
    return `<span style="color:${color}">${formatCredit(quota.remaining)} / ${formatCredit(quota.limit)}</span> <span style="color:#64748b;font-size:0.75rem">(Free 实报${windowText ? " · " + windowText : ""})</span>`;
  }
  if (quota && quota.estimated) {
    const pct = quota.pctRemaining;
    const color = pct <= 10 ? "#fb7185" : pct <= 30 ? "#f59e0b" : "#94a3b8";
    // "≈" is the point of the whole exercise: the number is a sense of scale, never a
    // balance the upstream actually reported.
    const usage = quota.observed ? formatCredit(quota.used) : "未统计";
    const windowText = quota.windowHours ? `滚动 ${quota.windowHours}h` : "";
    return `<span style="color:${color}">≈ ${usage} / ${formatCredit(quota.limit)}</span> <span style="color:#64748b;font-size:0.75rem">(Free 估算${windowText ? " · " + windowText : ""})</span>`;
  }
  if (quota && quota.quotaUnavailable) {
    return `<span style="color:#94a3b8">未知</span> <span style="color:#64748b;font-size:0.75rem">(xAI 未下发 Build 数值配额)</span>`;
  }
  if (quota && quota.unmetered) {
    // Unmetered is a verdict, not a missing number: the channel is billed by
    // rate limit rather than a balance, so the cell says so instead of showing
    // a dash. The reason is the tooltip, because the server's sentence is far
    // too long to sit next to the number.
    const reason = quota.note || "该渠道按速率限制计费，不提供数值额度";
    return `<span style="color:#94a3b8" title="${escapeHtml(reason)}">未计量</span> <span style="color:#64748b;font-size:0.75rem">(按速率限制)</span>`;
  }
  if (quota && quota.unknown) {
    const hint = normalizeAccountType(acc) === "workbuddy"
      ? "WorkBuddy 计量接口未返回数据"
      : "该渠道暂无可用额度接口";
    return `<span>未知</span> <span style="color:#64748b;font-size:0.75rem">(${hint})</span>`;
  }
  if (quota && quota.workbuddy) {
    const pct = quota.pctRemaining;
    const color = pct <= 10 ? "#fb7185" : pct <= 30 ? "#f59e0b" : "#34d399";
    const resetText = quota.resetAt ? `<div style="color:#64748b;font-size:0.75rem">${formatQuotaReset(quota.resetAt)}</div>` : "";
    return `<span style="color:${color}">${formatCredit(quota.remaining)} / ${formatCredit(quota.limit)}</span> <span style="color:#64748b;font-size:0.75rem">(剩余)</span>${resetText}`;
  }
  if (quota) {
    const pct = quota.pctRemaining;
    const color = pct <= 10 ? "#fb7185" : pct <= 30 ? "#f59e0b" : "#34d399";
	if (quota.weeklyPercent) {
	  return `<span style="color:${color}">${quota.remaining.toLocaleString()}%</span> <span style="color:#64748b;font-size:0.75rem">/ 100% (周度剩余)</span>`;
	}
	return `<span style="color:${color}">${quota.remaining.toLocaleString()} / ${quota.limit.toLocaleString()}</span> <span style="color:#64748b;font-size:0.75rem">(剩余)</span>`;
  }
  return `<span style="color:#64748b">-</span>`;
}

function buildStatusMarkup(acc, badge) {
  return `<span class="tag" title="${escapeHtml(badge.tip || "")}" style="background:${badge.bg};color:${badge.color};border:none;">${escapeHtml(badge.text)}</span>${buildCooldownMarkup(acc)}`;
}

function renderAccountsMobile(container, pageItems, total, totalPages) {
  container.innerHTML = "";
  const list = document.createElement("div");
  list.className = "accounts-mobile-list";

  const fragment = document.createDocumentFragment();
  pageItems.forEach((acc) => {
    const badge = statusBadge(acc);
    const tokenDisplay = formatTokenDisplay(acc);
    const card = document.createElement("article");
    card.className = "account-mobile-card";
    card.innerHTML = `
      <div class="account-mobile-head">
        <label class="account-mobile-check">
          <input type="checkbox" class="row-checkbox" data-action="row-select" data-id="${encodeData(acc.id)}">
          <span>#${escapeHtml(acc.id === null || acc.id === undefined ? "" : String(acc.id))}</span>
        </label>
        <div class="account-mobile-actions">
          <button type="button" class="action-icon" data-action="edit" data-id="${encodeData(acc.id)}" title="编辑账号设置与凭据">编辑</button>
          <button type="button" class="action-icon" data-action="refresh" data-id="${encodeData(acc.id)}" title="立即同步状态与额度">刷新</button>
          <button type="button" class="action-icon is-danger" data-action="delete" data-id="${encodeData(acc.id)}" title="删除该账号">删除</button>
        </div>
      </div>
      <div class="account-mobile-identity">${escapeHtml(accountIdentityPrimary(acc))}</div>
      <div class="account-mobile-token">
        <span class="token-text" title="${escapeHtml(tokenDisplay)}">${escapeHtml(tokenDisplay)}</span>
      </div>
      <div class="account-mobile-grid">
        <div class="account-mobile-item">
          <span class="account-mobile-label">状态</span>
          <div class="account-mobile-inline">${buildStatusMarkup(acc, badge)}</div>
        </div>
        <div class="account-mobile-item">
          <span class="account-mobile-label">等级</span>
          <div class="account-mobile-inline">${buildSubscriptionMarkup(acc)}</div>
        </div>
        ${clinePageOnly() ? "" : `
        <div class="account-mobile-item">
          <span class="account-mobile-label">配额</span>
          <div class="account-mobile-value">${buildQuotaMarkup(acc)}</div>
        </div>`}
        <div class="account-mobile-item">
          <span class="account-mobile-label">今日/累计 Tokens</span>
          <div class="account-mobile-value">${buildTokensMarkup(acc)}</div>
        </div>
        <div class="account-mobile-item">
          <span class="account-mobile-label">调用</span>
          <span class="account-mobile-value">${escapeHtml(String(accountUsageCounter(acc)))} · ${escapeHtml(acc.last_used_at && !acc.last_used_at.startsWith("0001") ? formatTime(acc.last_used_at) : "未调用")}</span>
        </div>
        <div class="account-mobile-item">
          <span class="account-mobile-label">创建时间</span>
          <span class="account-mobile-value">${buildCreatedMarkup(acc)}</span>
        </div>
        ${buildMobileEmailMarkup(acc)}
      </div>
    `;
    fragment.appendChild(card);
  });

  list.appendChild(fragment);
  container.appendChild(list);

  const paginationInfo = domCache.paginationInfo || document.getElementById("paginationInfo");
  paginationInfo.textContent = `共 ${total} 条记录，第 ${currentPage}/${totalPages} 页`;
  renderPagination(currentPage, totalPages);
  updateSelectedCount();

  bindAccountActions(container);
}

// Desktop rows and mobile cards carry the same delegated actions. The mobile
// view has no select-all checkbox, but its row selection uses this same handler.
function bindAccountActions(container) {
  container.onclick = (e) => {
    const actionEl = e.target.closest("[data-action]");
    if (!actionEl || !container.contains(actionEl)) return;
    const id = parseDataId(actionEl.dataset.id || "");
    if (actionEl.dataset.action === "edit") editAccount(id);
    if (actionEl.dataset.action === "refresh") refreshToken(id);
    if (actionEl.dataset.action === "delete") deleteAccount(id);
  };
  container.onchange = (e) => {
    const target = e.target;
    if (!(target instanceof HTMLInputElement)) return;
    if (target.dataset.action === "row-select") updateSelectedCount();
    if (target.dataset.action === "select-all") toggleSelectAll(target.checked);
  };
}

function renderPagination(current, total) {
  const container = domCache.paginationControls || document.getElementById("paginationControls");
  if (!container) return;

  container.innerHTML = "";
  const appendButton = (label, page, disabled, activeClass, extraStyle) => {
    const btn = document.createElement("button");
    btn.className = `btn ${activeClass}`.trim();
    btn.dataset.page = String(page);
    btn.disabled = disabled;
    btn.textContent = label;
    btn.style.padding = "4px 10px";
    if (extraStyle) {
      Object.keys(extraStyle).forEach((key) => {
        btn.style[key] = extraStyle[key];
      });
    }
    container.appendChild(btn);
  };

  // First & Prev
  appendButton("首页", 1, current === 1, "btn-outline");
  appendButton("上一页", current - 1, current === 1, "btn-outline");

  // Page Numbers (simplified logic: show surrounding)
  let startPage = Math.max(1, current - 2);
  let endPage = Math.min(total, startPage + 4);
  if (endPage - startPage < 4) {
    startPage = Math.max(1, endPage - 4);
  }

  for (let i = startPage; i <= endPage; i++) {
    const activeClass = i === current ? 'btn-primary' : 'btn-outline';
    appendButton(String(i), i, false, activeClass, { minWidth: "32px", justifyContent: "center" });
  }

  // Next & Last
  appendButton("下一页", current + 1, current === total, "btn-outline");
  appendButton("末页", total, current === total, "btn-outline");
  container.onclick = (e) => {
    const btn = e.target.closest("button[data-page]");
    if (!btn || !container.contains(btn) || btn.disabled) return;
    const page = parseInt(btn.dataset.page, 10);
    if (!Number.isNaN(page)) goToPage(page);
  };
}

function goToPage(page) {
  if (page < 1) return;
  // We can't check 'total' easily here without storing it or querying DOM
  // But renderAccounts will clamp it.
  currentPage = page;
  renderAccounts();
}

// Filter by platform
function filterByPlatform(platform) {
  platform = String(platform || "").trim().toLowerCase();
  currentPlatform = platform;
  currentPage = 1; // Reset to first page
  // Keep the modal's account type in step with the selected tab. The hidden
  // field is the only source of truth for "which provider am I adding", and it
  // must never silently fall back to the default channel when a platform tab is
  // selected.
  setAccountModalType(platformAccountType(platform));
  document.querySelectorAll("#platformFilters .tab-item").forEach(btn => {
    // The key, not the label: the strip renders the provider label while the tab
    // carries its key, so comparing textContent left the whole strip unselected
    // after a click.
    const key = btn.dataset.platform ? decodeURIComponent(btn.dataset.platform) : "";
    btn.classList.toggle("active", key === platform);
  });
  const subtitle = document.getElementById("pageSubtitle");
  if (subtitle) {
    subtitle.textContent = currentPlatform ? `管理您的 ${currentPlatformLabel()} API 凭证` : "管理您的所有 API 凭证";
  }
  renderAccounts();
}

// Update page size
function updatePageSize(size) {
  pageSize = parseInt(size);
  currentPage = 1;
  renderAccounts();
}

// Update statistics
function updateStats() {
  const total = accounts.length;
  // The sidebar and this page's 状态异常 stat must be the same number on every
  // page. isSidebarAccountAbnormal (common.js) is the single predicate; the
  // page-local isAccountAbnormal used to compute its own verdict, so the same
  // sidebar read differently on 账号管理 and on 运维总览.
  const abnormal = accounts.filter(isSidebarAccountAbnormal).length;
  const normal = Math.max(0, total - abnormal);

  document.getElementById("totalAccounts").textContent = total;
  document.getElementById("enabledAccounts").textContent = normal;
  document.getElementById("disabledAccounts").textContent = abnormal;

  // Attempt to update selected if element exists (it should)
  updateSelectedCount();

  // Update sidebar footer
  const footerTotal = document.getElementById("footerTotal");
  if (footerTotal) footerTotal.textContent = total;

  const footerNormal = document.getElementById("footerNormal");
  if (footerNormal) footerNormal.textContent = normal;

  const footerAbnormal = document.getElementById("footerAbnormal");
  if (footerAbnormal) footerAbnormal.textContent = abnormal;
}

// Update selected count
function updateSelectedCount() {
  const checked = document.querySelectorAll(".row-checkbox:checked").length;
  const el = document.getElementById("selectedCount");
  if (el) el.textContent = checked;
  const batchBtn = document.getElementById("batchDeleteBtn");
  if (batchBtn) {
    // Disabled styling comes from the button's own state, not from inline colours
    // that fought the design system.
    batchBtn.disabled = checked === 0;
  }
}

// Toggle select all
function toggleSelectAll(checked) {
  document.querySelectorAll(".row-checkbox").forEach(cb => cb.checked = checked);
  updateSelectedCount();
}

// Open modal
function openModal(account = null) {
  stopWorkBuddyLogin();
  stopQoderLogin();
  stopClineLogin();
  const modal = document.getElementById("accountModal");
  const title = document.getElementById("modalTitle");
  const form = document.getElementById("accountForm");
  const typeEl = document.getElementById("accountType");
  stopGrokDeviceLogin(true);
  resetGrokDeviceLoginStatus();

  const finalizeModal = () => {
    applyTokenLabels(typeEl ? typeEl.value : getActiveAccountType());
    modal.classList.add("active");
    modal.style.display = "flex";
  };

  const applyValues = () => {
    // Every branch below sets the modal type via setAccountModalType; the
    // credential-mode UI must follow THAT type, never the ambient platform tab.
    // (Reading the tab here left a Grok edit without its device-login button when
    // the list was showing another channel.)
    let modalType = "";
    if (account) {
      title.textContent = "编辑账号";
      document.getElementById("accountId").value = account.id;
      modalType = normalizeAccountType(account);
      setAccountModalType(modalType);
      document.getElementById("enabled").checked = account.enabled;
    } else {
      title.textContent = "添加账号";
      form.reset();
      document.getElementById("accountId").value = "";
      // The platform the operator explicitly selected wins over the ambient list
      // default and over any stale hidden field.
      modalType = selectedPlatformAccountType(typeEl);
      setAccountModalType(modalType);
      document.getElementById("enabled").checked = true;
    }
    // The switch above is assigned, not clicked: without this its paint would depend on
    // the stylesheet's :has() fallback, which browsers without :has() ignore.
    if (typeof window.syncToggleStates === "function") window.syncToggleStates();
    applyCredentialModeUI(modalType || normalizeAccountType({ account_type: typeEl?.value || getActiveAccountType() }));
  };

  applyValues();
  finalizeModal();
}

// WorkBuddy official login lifecycle. The login only ever starts from an
// explicit click on "使用 WorkBuddy 官方网页登录" (workbuddy-auth.js owns the
// popup); opening the modal must never navigate the operator anywhere.
function stopOfficialLogin(provider, statusId, linkId = "") {
  const login = globalThis[provider];
  if (login && typeof login.stop === "function") login.stop();
  const statusNode = document.getElementById(statusId);
  if (statusNode) {
    statusNode.hidden = true;
    statusNode.textContent = "";
    if (statusNode.classList) statusNode.classList.remove("is-active", "is-error");
  }
  if (linkId) {
    const linkNode = document.getElementById(linkId);
    if (linkNode) linkNode.hidden = true;
  }
}

function stopWorkBuddyLogin() { stopOfficialLogin("WorkBuddyLogin", "workbuddyLoginStatus"); }
function stopQoderLogin() { stopOfficialLogin("QoderLogin", "qoderLoginStatus", "qoderLoginLink"); }
function stopClineLogin() { stopOfficialLogin("ClineLogin", "clineLoginStatus", "clineLoginLink"); }

// Close modal
function closeModal() {
  stopGrokDeviceLogin(true);
  resetGrokDeviceLoginStatus();
  stopWorkBuddyLogin();
  stopQoderLogin();
  stopClineLogin();
  const modal = document.getElementById("accountModal");
  modal.classList.remove("active");
  modal.style.display = "none";
}

// Save settings for an existing account. New accounts use official login only.
async function saveAccount(e) {
  e.preventDefault();
  const id = document.getElementById("accountId").value;
  const type = document.getElementById("accountType").value;
  if (!id) {
    const label = accountTypeLabel(type);
    showToast(`请使用「使用 ${label} 官方网页登录」添加账号`, "error");
    return;
  }
  const existing = accounts.find((account) => String(account.id) === String(id));
  const data = {
    account_type: type,
    weight: existing ? (parseInt(existing.weight, 10) || 1) : 1,
    enabled: document.getElementById("enabled").checked,
  };
  if (type === "grok") {
    data.credential_type = "oauth";
    data.grok_provider = "build";
  }
  try {
    const res = await fetch(`/api/accounts/${id}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(data),
    });
    if (!res.ok) throw new Error(extractAdminErrorDetail(await res.text()));
    closeModal();
    loadAccounts();
    showToast("保存成功");
  } catch (err) {
    showToast("保存失败: " + err.message, "error");
  }
}

// Edit account
function editAccount(id) {
  const account = accounts.find((a) => a.id === id);
  if (account) openModal(account);
}

// Refresh token
async function refreshToken(id) {
  const actionText = "刷新";
  showToast(`正在${actionText}账号信息...`, "info");
  await checkAccount(id, false, actionText);
}

// Delete account
async function deleteAccount(id) {
  if (!confirm("确定要删除这个账号吗？")) return;
  try {
    const res = await fetch(`/api/accounts/${id}`, { method: "DELETE" });
    if (!res.ok) throw new Error(await res.text());
    showToast("删除成功");
    loadAccounts();
  } catch (err) {
    showToast("删除失败: " + err.message, "error");
  }
}

function parseDataId(value) {
  const decoded = decodeData(value);
  if (decoded === "") return "";
  const num = Number(decoded);
  return Number.isNaN(num) ? decoded : num;
}

function formatTokenDisplay(acc) {
  if (typeof acc?.has_credential === "boolean") return acc.has_credential ? '凭证已配置' : '待登录';
  const type = normalizeAccountType(acc);
  if (type === 'grok' && isSidebarGrokOAuthAccount(acc)) {
    const accessToken = String(acc.oauth_access_token || "");
    if (accessToken) {
      return accessToken.length > 20
        ? accessToken.substring(0, 8) + '...' + accessToken.substring(accessToken.length - 8)
        : accessToken;
    }
    return 'OAuth 已配置';
  }
  const token = acc.token;
  if (token) {
    if (token.length > 30) {
      if (type === 'grok') {
        return token.substring(0, 8) + '...' + token.substring(token.length - 8);
      }
      return token.substring(0, 30) + '...';
    }
    return token;
  }
  if (type === 'workbuddy') {
    // The signed-in address identifies both the account and the login; the token
    // itself is deliberately not shown for this channel.
    return workBuddyIdentityLabel(acc);
  }
  if (type === 'qoder') {
    // Same contract as WorkBuddy: the signed-in identity is what tells two
    // Qoder logins apart, and the device credential is never shown.
    return qoderIdentityLabel(acc);
  }
  if (acc.session_id) {
    return acc.session_id.substring(0, 30) + '...';
  }
  return '-';
}

// workBuddyIdentityLabel renders the account identity: the signed-in address when
// the profile was fetched, otherwise a short uid, otherwise the access token tail.
function workBuddyIdentityLabel(acc) {
  const email = String(acc?.email || "").trim();
  if (email) return email;
  const uid = String(acc?.workbuddy_uid || "").trim();
  if (uid) return uid.length > 20 ? `uid ${uid.substring(0, 8)}...${uid.substring(uid.length - 4)}` : `uid ${uid}`;
  const token = String(acc?.workbuddy_access_token || "").trim();
  if (token) {
    return token.length > 16 ? `token ${token.substring(0, 6)}...${token.substring(token.length - 6)}` : token;
  }
  return "-";
}


// qoderIdentityLabel renders the account identity: the signed-in address when
// the profile was fetched, otherwise the display name, otherwise a short uid.
function qoderIdentityLabel(acc) {
  const email = String(acc?.email || "").trim();
  if (email) return email;
  const name = String(acc?.name || "").trim();
  if (name && name !== "qoder-login") return name;
  const uid = String(acc?.qoder_user_id || "").trim();
  if (uid) return uid.length > 20 ? `uid ${uid.substring(0, 8)}...${uid.substring(uid.length - 4)}` : `uid ${uid}`;
  return "-";
}

// Export accounts
function exportAccounts() {
  window.location.href = "/api/export";
}

// Load accounts on page load
document.addEventListener('DOMContentLoaded', async () => {
  await window.OrchidsProviderRegistry?.ready;
  initDOMCache();
  loadAccounts();
  const typeSelect = document.getElementById("accountType");
  if (typeSelect) {
    applyTokenLabels(typeSelect.value);
  }
});
