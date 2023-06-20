package inner

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"myrepo/internal/chromedphandler"
	"myrepo/internal/elastichandler"
	"myrepo/internal/headlesshandler"
	"myrepo/internal/mongodbhandler"
	"myrepo/internal/nsqhandler"
	"myrepo/internal/urlhandler"
	"myrepo/internal/utility"
	"myrepo/internal/workerhandler"
	"myrepo/svc/worker/headless"
	"golang.org/x/sync/semaphore"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//CheckDB accepts the standard options, and operates a top level context, however it loops until all URLS are found
// based on the initial seed URLS.
//If a seed is a nested link "https://x.com/aboutus", it will parse upwards as well.
//Internal links only, not external.
//Redirects are also stored and sanatized to prevent endless loops
//urls with query params ?id="bob" are stripped before entering as internal links to prevent useless loop. This misses
// some sites that use query params for rendering content i.e.
//https://appexchange.salesforce.com/appxListingDetail?listingId=a0N30000000q64KEAQ&channel=featured&placement=a0d3A000007VqCzQAK
func CheckDB(w workerhandler.Worker, connections workerhandler.Connections, opts []chromedp.ExecAllocatorOption) bool {
	client, err := mongo.NewClient(options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = client.Connect(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Disconnect(ctx)
	err = client.Ping(ctx, readpref.Primary())
	if err != nil {
		log.Fatal(err)
	} else {
		fmt.Println("connected")
	}
	//make sure and elastic index exists
	elastichandler.IndexCheck("elastic" + w.Elasticname)

	crawlinnerDatabase := client.Database("mongodb" + w.Mongodbname)
	workingCollection := crawlinnerDatabase.Collection("mongodb" + w.Mongodbname)
	loopNum := 0
	for {
		seedURLS := make(map[string]string)
		//ATR arrayToRun is the results of checking mongo for the initial URLS, scrubing them to the host name,
		//comparing them to the links found in mongo and setting ATR to what needs to be done
		var ATR []string
		active := mongodbhandler.LoopDB(&w, connections, workingCollection, ctx, &ATR, &seedURLS, loopNum)
		if active == false || loopNum == 20 {
			break
		}
		w.SeedURLs = seedURLS
		BrowserSplit(ATR, w, connections, opts)
		time.Sleep(time.Second * 8)
		loopNum++

	}
	return true
}

//BrowserSplit Creates Parent Browser context for pushing work and connections into
func BrowserSplit(dbURLs []string, w workerhandler.Worker, connections workerhandler.Connections, opts []chromedp.ExecAllocatorOption) bool {

	browserCount := 1
	ss := utility.SliceToSliceSlice(dbURLs, browserCount)

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

//Split is an individual browser context, tied to master context, used to operate each tab.
//Cancellation propagates up, but not until all tabs and browser are completed
func Split(w workerhandler.Worker, connections workerhandler.Connections, tabURLS []string, wg *sync.WaitGroup) {
	browserCtx := connections.Ctx
	nsqp := connections.Nsqp
	defer func() {
		wg.Done()
	}()
	var wgi sync.WaitGroup

	//Passing all the URLs for this browser into a channel
	PairChan := make(chan workerhandler.URLSeedChan, len(tabURLS))

	for _, j := range tabURLS {
		URLSeedChan := new(workerhandler.URLSeedChan)
		for k, v := range w.SeedURLs {
			if strings.Contains(j, v) {
				URLSeedChan.SeedURL = k
				URLSeedChan.URL = j
			}
		}
		PairChan <- *URLSeedChan
	}
	// @@@ NUMBER OF TABS
	tabCount := 30
	sema := semaphore.NewWeighted(int64(tabCount))

	for tab := 1; tab <= tabCount; tab++ {
		wgi.Add(1)
		err := sema.Acquire(context.Background(), 1)
		if err != nil {
			fmt.Println(err)
		}
		ctx, _ := chromedp.NewContext(browserCtx)
		connections := workerhandler.Connections{
			Ctx:    ctx,
			Nsqp:   nsqp,
			TabNum: tab,
		}
		tabContent := workerhandler.TabContent{
			Command: w.Command,
			Params:  w.Params,
			Outputs: w.Outputs,
		}

		go func() {
			Manager(&wgi, connections, PairChan, tabContent)
			sema.Release(1)
		}()
		//slow down the next tab creation

	}
	close(PairChan)
	fmt.Println("CLOSED JOBS")
	wgi.Wait()
}

//Manager the work coming into manager is distributed across a channel, wg closes once work is done.
func Manager(wgi *sync.WaitGroup, connections workerhandler.Connections, PairChan chan workerhandler.URLSeedChan, tabContent workerhandler.TabContent) {
	i := 0
	defer func() {
		wgi.Done()
	}()
	//ranges over all the values sent down via channel
	for j := range PairChan {

		i++
		Default(j.URL, j.SeedURL, connections, tabContent)

	}
}

//Default visits the page and grabs url,redirect,links,title and other details
func Default(URL string, seedurl string, tabContext workerhandler.Connections, tabContent workerhandler.TabContent) {
	if tabContent.Params.Speed != "" {
		s, _ := strconv.Atoi(tabContent.Params.Speed)
		time.Sleep(time.Millisecond * time.Duration(s))
	}
	start := time.Now()
	var redir, pageTitle, urlORredir, str string
	var nodes []*cdp.Node

	err := chromedp.Run(
		tabContext.Ctx,
		headless.RunWithTimeOut(&tabContext.Ctx, 255, chromedp.Tasks{
			chromedp.Navigate(URL),
			chromedp.Location(&redir),
			chromedp.Nodes("a", &nodes),
			chromedp.Title(&pageTitle),
			chromedp.ActionFunc(func(ctx2 context.Context) error {
				time.Sleep(time.Millisecond * 2000)
				node, err := dom.GetDocument().Do(ctx2)
				if err != nil {
					return err
				}
				str, err = dom.GetOuterHTML().WithNodeID(node.NodeID).Do(ctx2)

				return err
			}),
		}),
	)
	redirected := false
	if redir != URL {
		fmt.Println("REDIRECT OCCURRED", URL, "-->", redir)
		urlORredir = redir
		redirected = true
	} else {
		urlORredir = URL
	}
	//fmt.Println(">>URLORREDIR", urlORredir)
	noError := headless.ResponseError(err, redir, urlORredir, URL)

	fqdn, host := urlhandler.SchemeHostSplit(urlORredir)
	fmt.Println("FQDN IS", fqdn)
	fmt.Println("HOST IS", host)
	fmt.Printf("length of str is %v", len(str))

	accessDenied := strings.HasPrefix(str, "<html><head>\n<title>Access Denied</title>")
	if accessDenied == true {
		fmt.Println("we have been blocked on this site")
	}
	if nodes != nil && noError == true && accessDenied != true {
		URLS, urlsIQS := headlesshandler.NodeCheck(nodes, fqdn, host)
		ints, exts := urlhandler.IntExtSort(fqdn, URLS)
		fmt.Println("\n")
		//for i, x := range ints {
		//	fmt.Println("##", i, "    XX", x)
		//}
		fmt.Println(len(ints), "-------", len(exts))

		//for i, x := range exts {
		//	fmt.Println("##", i, "    XX", x)
		//}
		fmt.Println("\n")
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
			SeedURL:         seedurl,
			URL:             urlORredir,
			InternalLinks:   ints,
			InternalLinksQP: intqs,
			ExternalLinks:   exts,
			ExternalLinksQP: extqs,
			Redirect:        redirected,
			RedirectFrom:    URL,
			RedirectTo:      urlORredir,
			Visited:         true,
			Method:          "headlessinner",
			PageTitle:       pageTitle,

			//TContent:      s,
		}
		jsonData, err := json.Marshal(h)
		if err != nil {
			fmt.Println("error marshaling")
			log.Println(err)
		}
		dataM := nsqhandler.NSQPub{Nsqp: tabContext.Nsqp, JSONData: jsonData, ChanName: "mongodbheadlessinner"}
		nsqhandler.NsqPublish(dataM)
		//fmt.Println("published to mongodbheadlessinner")

		//if URL+"/" != urlORredir || URL+"/" != fqdn {
		//	if redirected == true {
		//		redirectDoc := mongodbhandler.CrawlRedirect{
		//			UUID:         specid,
		//			ElasticName:  tabContent.Elasticname,
		//			MongodbName:  tabContent.Mongodbname,
		//			SeedURL:      seedurl,
		//			URL:          URL,
		//			RedirectFrom: URL,
		//			RedirectTo:   urlORredir,
		//			Visited:      true,
		//			Method:       "crawlpage",
		//		}
		//		jsonData, err := json.Marshal(redirectDoc)
		//		if err != nil {
		//			fmt.Println("error marshaling redirectDoc")
		//			log.Println(err)
		//		}
		//		data := nsqhandler.NSQPub{Nsqp: tabContext.Nsqp, JSONData: jsonData, ChanName: "mongodbheadlessredir"}
		//		nsqhandler.NsqPublish(data)
		//		fmt.Println("published to mongodbheadlessredir")
		//	}
		//}

		e := elastichandler.ElasticDoc{
			UUID:            specid,
			ElasticName:     tabContent.Elasticname,
			MongodbName:     tabContent.Mongodbname,
			TLD:             host,
			SeedURL:         fqdn,
			URL:             urlORredir,
			InternalLinks:   ints,
			InternalLinksQP: intqs,
			ExternalLinks:   exts,
			ExternalLinksQP: extqs,
			TextContent:     s,
			Method:          "headlessinner",
			PageTitle:       pageTitle,
			Timestamp:       sec,
		}
		jsonDatae, err := json.Marshal(e)
		if err != nil {
			fmt.Println("error marshaling")
			log.Println(err)
		}
		//if elasticname != "" {
		dataE := nsqhandler.NSQPub{Nsqp: tabContext.Nsqp, JSONData: jsonDatae, ChanName: "elasticheadlessinner"}
		nsqhandler.NsqPublish(dataE)
		//fmt.Println("published to elasticheadlessinner")
		//fmt.Println("URL", URL)
	} else if nodes != nil && noError == false {
		fmt.Println("# Nodes length was #", len(nodes), "on", URL, "-->", redir, "-->urlORredir", urlORredir)
	} else if nodes == nil {
		fmt.Println("# unable to capture due to error", URL, "-->")
	} else if accessDenied == true {
		fmt.Println("SKIPPED ENTERING", URL)
	} else {
		fmt.Println("# somehow we got to this else. Nodes length was #", len(nodes), "on", URL, "-->", redir, "-->urlORredir", urlORredir)
	}
	duration := time.Since(start)

	fmt.Printf("body is %v kb \n", len(str)/1000)
	fmt.Println("URL", URL, " DURATION", duration)

}
