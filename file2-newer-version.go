package page

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
	"goauld/internal/chromedphandler"
	"goauld/internal/elastichandler"
	"goauld/internal/headlesshandler"
	"goauld/internal/mongodbhandler"
	"goauld/internal/nsqhandler"
	"goauld/internal/urlhandler"
	"goauld/internal/utility"
	"goauld/internal/workerhandler"
	"goauld/svc/worker/headless"
	"golang.org/x/sync/semaphore"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//BrowserSplit Creates Parent Browser context for pushing work and connections into
func BrowserSplit(w workerhandler.Worker, connections workerhandler.Connections, opts []chromedp.ExecAllocatorOption) bool {
	browserCount := 1
	ss := utility.SliceToSliceSlice(w.Urls, browserCount)

	var wg sync.WaitGroup
	for _, s := range ss {
		wg.Add(1)
		ctx, cancel := chromedphandler.GenContext(opts)
		defer cancel() // <- THIS IS OK!!!
		//Reason: We're looping through a slice, creating a browser context and passing the context inside a go routine.
		//We can't cancel outside the for loop because they're generated inside.
		//--Running it as a go func will cause immediate termination.
		//go func(cancel context.CancelFunc) {
		//	defer cancel()
		//}(cancel)
		//--Running it as a plain func will cause it to close early also
		//func(cancel context.CancelFunc) {
		//	defer cancel()
		//}(cancel)
		connections.Ctx = ctx
		go Split(w, connections, s, &wg)

	}
	wg.Wait()
	return true
}

// Split is an individual browser context, tied to master context, used to operate each tab.
// Cancellation propagates up, but not until all tabs and browser are completed
func Split(w workerhandler.Worker, connections workerhandler.Connections, tabURLS []string, wg *sync.WaitGroup) {
	browserCtx := connections.Ctx
	nsqp := connections.Nsqp
	defer func() {
		wg.Done()
	}()
	var wgi sync.WaitGroup

	//Passing all the URLs for this browser into a channel
	urlChan := make(chan string, len(tabURLS))
	for _, j := range tabURLS {
		urlChan <- j
	}
	// @@@ NUMBER OF TABS
	tabCount := 45
	sema := semaphore.NewWeighted(int64(tabCount))
	//var wg0 sync.WaitGroup
	// Flip through the tabs, create a new tab specific context, shadow the connections.
	//outs := make(chan string)
	//go func() {
	//	defer wg0.Done()
	//	wg0.Add(1)
	for tab := 1; tab <= tabCount; tab++ {
		wgi.Add(1)
		time.Sleep(time.Millisecond * 300)
		ctx, _ := chromedp.NewContext(browserCtx)
		connections := workerhandler.Connections{
			Ctx:     ctx,
			Nsqp:    nsqp,
			TabNum:  tab,
			TimeOut: 20,
		}
		tabContent := workerhandler.TabContent{
			Command: w.Command,
			Params:  w.Params,
			Outputs: w.Outputs,
		}
		//Blocking semaphore
		err := sema.Acquire(context.Background(), 1)
		if err != nil {
			fmt.Println(err)
		}
		go func() {

			Manager(&wgi, connections, urlChan, tabContent)

			sema.Release(1)

		}()
	}

	//}()
	//wg0.Wait()
	close(urlChan)

	//var outputs []string
	//for range outs {
	//	outputs = append(outputs, <-outs)
	//}

	wgi.Wait()
	//fmt.Println("CLOSED JOBS")
	//fmt.Println("\n\nMOMOMOMOMOOMMOMOOMOMMO\n\n\n I AM BROWSER OUTPUTS", len(outputs))
}
func Manager(wgi *sync.WaitGroup, connections workerhandler.Connections, urlChan chan string, tabContent workerhandler.TabContent) {
	i := 0
	defer func() {
		wgi.Done()
	}()
	//out := make(chan string)
	//var wg sync.WaitGroup

	//go func() {
	//	defer wg.Done()
	//wg.Add(1)
	for j := range urlChan {
		//slow down the next tab creation
		i++
		fmt.Println("worker", i, "started  job", j)
		log.Println("URL: ", j)
		_ = Default(j, connections, tabContent)
		fmt.Println("worker", i, "finished job", j)
		//in <- out
	}
	//close(out)
	//}()
	//wg.Wait()

	//var outputs []string
	//for range out {
	//	outputs = append(outputs, <-out)
	//}
	//for _, o := range outputs {
	//	outs <- o
	//}
	//fmt.Println("\n\n\n\n\n I AM", len(outputs))

}
func Default(url string, tabContext workerhandler.Connections, tabContent workerhandler.TabContent) error {
	if tabContent.Params.Speed != "" {
		s, _ := strconv.Atoi(tabContent.Params.Speed)
		time.Sleep(time.Millisecond * time.Duration(s))
	}
	start := time.Now()
	var redir, pageTitle, urlORredir, str string
	var nodes []*cdp.Node
	var noError bool

	err := chromedp.Run(
		tabContext.Ctx,
		headless.RunWithTimeOut(&tabContext.Ctx, time.Duration(tabContext.TimeOut), chromedp.Tasks{
			browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorDeny),
			chromedp.Navigate(url),
			chromedp.Location(&redir),
			chromedp.Nodes("a", &nodes),
			chromedp.Title(&pageTitle),
			chromedp.ActionFunc(func(ctx2 context.Context) error {
				time.Sleep(time.Millisecond * 5000)
				node, err := dom.GetDocument().Do(ctx2)
				if err != nil {
					return err
				}
				str, err = dom.GetOuterHTML().WithNodeID(node.NodeID).Do(ctx2)

				return err
			}),
		}),
	)

	if redir != url {
		fmt.Println("REDIRECT OCCURRED", url, "-->", redir)
		urlORredir = redir
	}

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
		//in <- url
		return err
	} else {
		noError = true
		fmt.Println("OK#1--> ", urlORredir, url)
	}

	scheme, host, err := urlhandler.SchemeHostSplitErr(urlORredir)
	if err != nil {
		fmt.Println("BAD URL on", urlORredir)
	} else {
		fmt.Printf("length of str is %v", len(str))

		accessDenied := strings.HasPrefix(str, "<html><head>\n<title>Access Denied</title>")
		if accessDenied == true {
			fmt.Println("we have been blocked on this site")
		}
		if nodes != nil && noError == true && accessDenied != true {
			URLS, urlsIQS := headlesshandler.NodeCheck(nodes, scheme, host)
			ints, exts := urlhandler.IntExtSort(urlORredir, URLS)
			intqs, extqs := urlhandler.IntExtSort(urlORredir, urlsIQS)
			doc, err := goquery.NewDocumentFromReader(strings.NewReader(str))
			if err != nil {
				fmt.Println("issue with go query")
				log.Fatal(err)
			}
			doc.Find("script").Remove()
			doc.Find("style").Remove()
			doc.Find("aria-hidden").Remove()
			doc.Find("aria-label").Remove()
			bodyText := doc.Find("body").Text()
			space := regexp.MustCompile(`\s{2,}`)
			s := space.ReplaceAllString(bodyText, "\n")
			//bodyHTML, err := doc.Find("body").Html()
			//if err != nil {
			//	fmt.Println("Error getting body html", err)
			//}

			id := uuid.New()
			specid := id.String()

			now := time.Now()
			sec := now.Unix()
			h := mongodbhandler.CrawlHit{
				UUID:            specid,
				ElasticName:     tabContent.Elasticname,
				MongodbName:     tabContent.Mongodbname,
				SeedURL:         url,
				URL:             urlORredir,
				InternalLinks:   ints,
				InternalLinksQP: intqs,
				ExternalLinks:   exts,
				ExternalLinksQP: extqs,
				Visited:         true,
				Method:          "headlesspage",
				PageTitle:       pageTitle,

				//TContent:      s,
			}
			jsonData, err := json.Marshal(h)
			if err != nil {
				fmt.Println("error marshaling")
				log.Println(err)
			}
			dataM := nsqhandler.NSQPub{Nsqp: tabContext.Nsqp, JSONData: jsonData, ChanName: "mongodbheadlesspage"}
			nsqhandler.NsqPublish(dataM)
			fmt.Println("published to mongodbheadlesspage")

			e := elastichandler.ElasticDoc{
				UUID:            specid,
				ElasticName:     tabContent.Elasticname,
				MongodbName:     tabContent.Mongodbname,
				TLD:             host,
				SeedURL:         url,
				URL:             urlORredir,
				InternalLinks:   ints,
				InternalLinksQP: intqs,
				ExternalLinks:   exts,
				ExternalLinksQP: extqs,
				TextContent:     s,
				HTMLContent:     "",
				Method:          "headlesspage",
				PageTitle:       pageTitle,
				Timestamp:       sec,
			}
			jsonDatae, err := json.Marshal(e)
			if err != nil {
				fmt.Println("error marshaling")
				log.Println(err)
			}
			//if elasticname != "" {
			dataE := nsqhandler.NSQPub{Nsqp: tabContext.Nsqp, JSONData: jsonDatae, ChanName: "elasticheadlesspage"}
			nsqhandler.NsqPublish(dataE)
			fmt.Println("published to elasticheadlesspage")
			fmt.Println("URL", url)
		} else if nodes != nil && noError == false {
			fmt.Println("# Nodes length was #", len(nodes), "on", url, "-->", redir, "-->urlORredir", urlORredir)
		} else if nodes == nil {
			fmt.Println("# unable to capture due to error", url, "-->")
		} else if accessDenied == true {
			fmt.Println("SKIPPED ENTERING", url)
		} else {
			fmt.Println("# somehow we got to this else. Nodes length was #", len(nodes), "on", url, "-->", redir, "-->urlORredir", urlORredir)
		}
		duration := time.Since(start)

		fmt.Printf("body is %v kb \n", len(str)/1000)
		fmt.Println("URL", url, " DURATION", duration)
	}
	return nil
}
