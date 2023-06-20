package headless

import (
	"context"
	"fmt"
	"github.com/chromedp/chromedp"
	"time"
)

type Technology struct {
	ID   int    `json:"id"`
	Tech string `json:"tech"`
}

func RunWithTimeOut(ctx *context.Context, timeout time.Duration, tasks chromedp.Tasks) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		timeoutContext, cancel := context.WithTimeout(ctx, timeout*time.Second)
		defer cancel()
		return tasks.Do(timeoutContext)
	}
}

func ResponseError(err error, redir string, urlORredir string, url string) bool {
	if err != nil {
		e := err.Error()
		switch e {
		case "page load error net::ERR_CERT_COMMON_NAME_INVALID",
			"page load error net::ERR_NAME_NOT_RESOLVED",
			"page load error net::ERR_CONNECTION_REFUSED",
			"page load error net::ERR_CONNECTION_CLOSED",
			"Could not find node with given id (-32000)",
			"page load error net::ERR_CERT_DATE_INVALID",
			"page load error net::ERR_CONNECTION_RESET",
			"page load error net::ERR_TIMED_OUT",
			"page load error net::ERR_TOO_MANY_REDIRECTS",
			"page load error net::ERR_CERT_AUTHORITY_INVALID":
			fmt.Println(err, "|", redir, "->", urlORredir, url)
		case "page load error net::ERR_NETWORK_CHANGED":
			fmt.Println("NETWORK CHANGED/CONNECTION DROP", err, redir, "->", urlORredir, url)
		default:
			fmt.Println("NEW ERROR TYPE", err, "|", urlORredir, url)
		}
		return false
	} else {
		fmt.Println("OK#1--> ", urlORredir, url)
		return true
	}
}
