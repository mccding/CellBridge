package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem/at"
)

func main() {
	number := "10086"
	if len(os.Args) > 1 && strings.TrimSpace(os.Args[1]) != "" {
		number = strings.TrimSpace(os.Args[1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	client, err := at.OpenSerial(ctx, "/dev/serial/by-id/usb-BAIWANG_Baiwang-if02-port0", 9600)
	if err != nil { panic(err) }
	defer client.Close()
	commands := []string{"AT", "AT+CMGF=0", "AT+CMGL=4", "ATD" + number + ";", "ATH"}
	for _, command := range commands {
		lines, exchangeErr := client.Exchange(ctx, command)
		fmt.Printf("command=%s err=%v lines=%q\n", command, exchangeErr, lines)
		if exchangeErr != nil { os.Exit(1) }
	}
}
