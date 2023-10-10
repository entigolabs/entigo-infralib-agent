package common

type Command int

const (
	RunCommand Command = iota
	BootstrapCommand
	MergeCommand
)

type LoggingLevel int

const (
	DevLoggingLvl LoggingLevel = iota
	ProdLoggingLvl
)
