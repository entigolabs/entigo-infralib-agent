package common

type Command string

const (
	RunCommand             Command = "run"
	BootstrapCommand       Command = "bootstrap"
	DestroyCommand         Command = "destroy"
	DeleteCommand          Command = "delete"
	UpdateCommand          Command = "update"
	SACommand              Command = "service-account"
	PullCommand            Command = "pull"
	AddCustomCommand       Command = "add-custom"
	DeleteCustomCommand    Command = "delete-custom"
	GetCustomCommand       Command = "get-custom"
	ListCustomCommand      Command = "list-custom"
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
