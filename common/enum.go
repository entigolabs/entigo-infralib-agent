package common

type Command string

const (
	RunCommand       Command = "run"
	BootstrapCommand Command = "bootstrap"
	DeleteCommand    Command = "delete"
	UpdateCommand    Command = "update"
	SACommand        Command = "service-account"
)

type LoggingLevel int

const (
	DevLoggingLvl LoggingLevel = iota
	ProdLoggingLvl
)
