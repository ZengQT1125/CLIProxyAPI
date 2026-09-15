package config

const (
	DefaultPanelGitHubRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
	DefaultAuthDir               = "~/.cli-proxy-api"
	DefaultAuthLoadWorkers       = 16
	MinAuthLoadWorkers           = 1
	MaxAuthLoadWorkers           = 64
	DefaultDiscoveryServiceType  = "_ai-gateway._tcp"
)
