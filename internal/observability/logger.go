package observability

import (
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func InitLogger(level string) {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC() }

	lvl, err := zerolog.ParseLevel(strings.ToLower(level))
	if err != nil || level == "" {
		lvl = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(lvl)
	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Caller().Logger()
}
