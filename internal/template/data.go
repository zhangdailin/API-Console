package template

// PageData represents the data passed to page templates
type PageData struct {
	Title     string
	AdminPath string
	ActiveTab string
	// AssetVersion is appended to every stylesheet and script URL as ?v=. It is a
	// content hash of the embedded assets (web.AssetVersion), so a build that
	// changes an asset also changes its URL and cannot be served from a browser
	// or CDN cache filled by an earlier deployment.
	AssetVersion   string
	LoginProviders []LoginProvider
}

type LoginProvider struct {
	Key        string
	GroupID    string
	Label      string
	ButtonID   string
	ButtonText string
	StatusID   string
	LinkID     string
	LinkTextID string
	HelpText   string
}

var accountLoginProviders = []LoginProvider{
	{
		Key: "workbuddy", GroupID: "workbuddyLoginGroup", Label: "官方 WorkBuddy 登录（国际版）",
		ButtonID: "workbuddyLoginButton", ButtonText: "使用 WorkBuddy 官方网页登录",
		StatusID: "workbuddyLoginStatus", LinkID: "workbuddyLoginLink", LinkTextID: "workbuddyLoginLinkText",
		HelpText: "在 www.workbuddy.ai 完成登录并授权本管理站点；服务端只保存 access/refresh token，读取不到密码。授权完成后自动保存账号并同步模型目录与额度，refreshToken 由服务端自动轮换。登录只在点击按钮后开始；编辑已有账号时也可用它重新授权。该渠道只支持官方登录，不提供手填凭证。",
	},
	{
		Key: "qoder", GroupID: "qoderLoginGroup", Label: "官方 Qoder 登录（设备授权流）",
		ButtonID: "qoderLoginButton", ButtonText: "使用 Qoder 官方网页登录",
		StatusID: "qoderLoginStatus", LinkID: "qoderLoginLink", LinkTextID: "qoderLoginLinkText",
		HelpText: "在 qoder.com 完成官方设备授权；服务端只保存 device access/refresh token，读取不到密码。授权完成后自动保存账号并同步模型目录，refreshToken 由服务端自动轮换。该渠道只支持官方登录，不提供 PAT（个人访问令牌）入口。",
	},
	{
		Key: "cline", GroupID: "clineLoginGroup", Label: "官方 Cline 登录（WorkOS 设备授权）",
		ButtonID: "clineLoginButton", ButtonText: "使用 Cline 官方网页登录",
		StatusID: "clineLoginStatus", LinkID: "clineLoginLink", LinkTextID: "clineLoginLinkText",
		HelpText: "在 WorkOS 官方页面完成 Cline 授权；服务端只保存 Cline access/refresh token，读取不到密码。授权完成后自动保存账号并同步模型目录，refreshToken 由服务端自动轮换。该渠道只支持官方登录，不提供手填凭证。",
	},
	{
		Key: "grok", GroupID: "grokDeviceLoginGroup", Label: "官方 Grok 登录（Build CLI OAuth）",
		ButtonID: "grokDeviceLoginButton", ButtonText: "使用 Grok 官方网页登录",
		StatusID: "grokDeviceLoginStatus", LinkID: "grokDeviceLoginLink", LinkTextID: "grokDeviceLoginLinkText",
		HelpText: "浏览器将在 xAI 官方页面完成授权；本服务只保存 OAuth access/refresh token（且不回显），不读取本地文件、Cookie 或密码，也不保存 id_token。授权完成会自动保存账号。",
	},
}
