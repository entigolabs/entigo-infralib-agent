package common

type Command string

const (
	RunCommand       Command = "run"
	BootstrapCommand Command = "bootstrap"
	DeleteCommand    Command = "delete"
	UpdateCommand    Command = "update"
)

type LoggingLevel int

const (
	DevLoggingLvl LoggingLevel = iota
	ProdLoggingLvl
)
