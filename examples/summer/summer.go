package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Jille/distwasm/distwasmapi"
)

func main() {
	distwasmapi.Setup(process)
}

func process(data []byte) ([]byte, error) {
	sp := strings.Split(string(data), "+")
	a, err := strconv.ParseInt(strings.TrimSpace(sp[0]), 10, 64)
	if err != nil {
		return nil, err
	}
	b, err := strconv.ParseInt(strings.TrimSpace(sp[1]), 10, 64)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprint(a + b)), nil
}
