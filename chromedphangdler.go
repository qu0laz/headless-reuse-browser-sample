package chromedphandler

import (
	"context"
	"fmt"
	"github.com/chromedp/chromedp"
)

func GenContext(opts []chromedp.ExecAllocatorOption) (context.Context, context.CancelFunc) {
	ctx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel = chromedp.NewContext(ctx)
	err := chromedp.Run(ctx)
	if err != nil {
		fmt.Println("TOP CONTEXT")
		fmt.Println("CONTEXT ERROR", err)
	}
	return ctx, cancel
}
