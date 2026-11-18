package config

import "errors"

var (
	ErrNoHome             = errors.New("no Kopi home directory: set KOPI_HOME or HOME")
	ErrUnknownKey         = errors.New("unknown config key")
	ErrInvalidValue       = errors.New("invalid config value")
	ErrUnsupportedVersion = errors.New("unsupported config version")
	ErrInvalidModel       = errors.New("invalid model: want provider/id")
	ErrUnsetVariable      = errors.New("unset variable with no default")
)
