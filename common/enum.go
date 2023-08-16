package common

type Command int

const (
	UpdateCommand Command = iota
)

type LoggingLevel int

const (
	DevLoggingLvl LoggingLevel = iota
	ProdLoggingLvl
)
