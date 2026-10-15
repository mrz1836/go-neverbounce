// Command quickstart verifies one address against the live NeverBounce API.
// It makes one billable call (1 credit) with the key in NEVERBOUNCE_API_KEY;
// the offline demo is in the parent directory (go run ./examples).
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/mrz1836/go-neverbounce"
)

func main() {
	// The application chooses where the key comes from; the SDK reads no environment.
	client, err := neverbounce.NewClient(os.Getenv("NEVERBOUNCE_API_KEY"))
	if err != nil {
		log.Fatal(err) // ErrInvalidConfig: no call was made
	}

	// One billable call (1 credit), whatever the result.
	res, err := client.CheckEmail(context.Background(), neverbounce.CheckEmailRequest{
		Email:       "avery.example@example.com",
		AddressInfo: true,
	})
	if err != nil {
		log.Fatal(err) // the check may already be billed: do not resend blindly
	}
	fmt.Println(res.Result, res.Flags)
}
