package common

type Command string

const (
	RunCommand       Command = "run"
	BootstrapCommand Command = "bootstrap"
	DestroyCommand   Command = "destroy"
	DeleteCommand    Command = "delete"
	UpdateCommand    Command = "update"
	SACommand        Command = "service-account"
	PullCommand      Command = "pull"
)

type LogLevel string

const (
	DebugLogLevel LogLevel = "debug"
	ProdLogLevel  LogLevel = "info"
	WarnLogLevel  LogLevel = "warn"
	ErrorLogLevel LogLevel = "error"
)
