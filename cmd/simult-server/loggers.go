package main

import (
	"sync/atomic"

	"github.com/simult/server/pkg/config"
	"github.com/simult/server/pkg/lb"
	"github.com/simult/server/pkg/logger"
)

var (
	errorLogger   logger.Logger = &logger.NullLogger{}
	warningLogger logger.Logger = &logger.NullLogger{}
	infoLogger    logger.Logger = &logger.NullLogger{}
	debugLogger   logger.Logger = &logger.NullLogger{}

	loggersInitialized uint32
)

func initializeLoggers(err, warn, info, dbg logger.Logger) {
	if !atomic.CompareAndSwapUint32(&loggersInitialized, 0, 1) {
		panic("loggers already initialized")
	}
	errorLogger = err
	warningLogger = warn
	infoLogger = info
	debugLogger = dbg

	config.InitializeLoggers(err, warn, info, dbg)
	lb.InitializeLoggers(err, warn, info, dbg)
}
