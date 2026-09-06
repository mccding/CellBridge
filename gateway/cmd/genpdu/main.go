package main
import (
 "fmt"
 "github.com/cellbridge/cellbridge/gateway/internal/sms"
)
func main(){
 segs, _ := sms.EncodeSubmitSegments("10010", "hello from gateway 10010 test", 0)
 fmt.Println(segs[0].PDU)
 fmt.Println(len(segs))
 for i, s:=range segs { fmt.Printf("%d %s\n", i, s.PDU) }
}
