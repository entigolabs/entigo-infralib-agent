package common

type Command string

const (
	RunCommand             Command = "run"
	BootstrapCommand       Command = "bootstrap"
	DeleteCommand          Command = "delete"
	UpdateCommand          Command = "update"
	SACommand              Command = "service-account"
	PullCommand            Command = "pull"
	MigratePlanCommand     Command = "migrate-plan"
	MigrateValidateCommand Command = "migrate-validate"
)

type LogLevel string

const (
	DebugLogLevel LogLevel = "debug"
	ProdLogLevel  LogLevel = "info"
	WarnLogLevel  LogLevel = "warn"
	ErrorLogLevel LogLevel = "error"
)
