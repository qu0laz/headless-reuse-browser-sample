package pipeline

import (
	"fmt"
	"github.com/chromedp/chromedp"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"myrepo/internal/nsqhandler"
	"myrepo/internal/workerhandler"
	getinner "myrepo/svc/worker/get/inner"
	"myrepo/svc/worker/get/page"
	"myrepo/svc/worker/headless/inner"
	page "myrepo/svc/worker/headless/page"
	"myrepo/svc/worker/headless/tech"
	"log"
)
type Worker struct {
	Command   `json:"Command"`
	Worker    string            `json:"Worker"`
	Urls      []string          `json:"URLs"`
	SeedURLs  map[string]string `json:"seedurls,omitempty"`
	Params    `json:"Params"`
	Outputs   `json:"Outputs"`
	Endpoints `json:"Endpoints"`
}
// Manager accepts all the potential input parameters and pushes the work to the appropriate function,
// once complete it returns true so the next file can be worked on. Is blocking
func Manager(w *workerhandler.Worker) bool {
	//Create a nsqProducer relative to this machine for passing data to
	nsqprod := nsqhandler.NsqProdLocal()

	//<--Mongo establish-->//
	client, err := mongo.NewClient(options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		log.Fatal(err)
	}

	//Maks sure an index is created for elastic search
	//elastichandler.IndexCheck("elastic" + w.Outputs.Elasticname)

	//Pass a mongo connection down stream for inner crawl functions- they loop to check what's not in mongo
	//Pass nsq for use at the final endpoint to publish to the ingress services.
	connections := workerhandler.Connections{
		Nsqp:        nsqprod,
		MongoClient: client,
	}

	// Set options for browser context
	cdpHeadless := true
	if w.Params.Headless == "true" {
		cdpHeadless = false
	}
	fmt.Println("HEADLESS IS \n\n", cdpHeadless)
	fmt.Println("PPP \n\n", w.Params.Headless)
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", cdpHeadless),
		chromedp.Flag("block-new-web-contents", true),
		chromedp.Flag("disable-gpu", false),
		chromedp.Flag("enable-automation", true),
		chromedp.Flag("disable-extensions", false),
	)
	//switching based on user defined command
	switch w.Cmd {
	case "headlesspage":
		fmt.Println("<<<<<<<<<<<1")
		return page.BrowserSplit(*w, connections, opts)
	case "headlessinner":
		fmt.Println("<<<<<<<<<<<2")
		return inner.CheckDB(*w, connections, opts)
	case "headlesstech":
		fmt.Println("<<<<<<<<<<<3")
		return tech.LoadApps(*w, connections, opts)
	case "getpage":
		fmt.Println("<<<<<<<<<<<4")
		return get.RequestSplit(*w, connections)
	case "getinner":
		fmt.Println("<<<<<<<<<<<5")
		return getinner.CheckDB(*w, connections)
	}
	return true
}
