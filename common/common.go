package common

import (
	"log"
	"runtime/debug"
)

func LogErr(err error) {
	log.Println(err)
	debug.PrintStack()
}
