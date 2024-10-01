package common

type Command int

const (
	RunCommand Command = iota
	BootstrapCommand
	DeleteCommand
)

type LoggingLevel int

const (
	DevLoggingLvl LoggingLevel = iota
	ProdLoggingLvl
)
