/***************************************************************************
 * COPYRIGHT (C) 2018, Rapid7 LLC, Boston, MA, USA.
 * This code is licensed under MIT license (see LICENSE for details)
 **************************************************************************/
package renderer

import (
	"github.com/mafredri/cdp/devtool"
	"github.com/mafredri/cdp/rpcc"
	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/protocol/network"
	"github.com/mafredri/cdp/protocol/page"
	"context"
	"encoding/json"
	"github.com/mafredri/cdp/protocol/target"
	"fmt"
	log "github.com/sirupsen/logrus"
	"time"
	"os"
	"strconv"
	"github.com/mafredri/cdp/session"
	"github.com/rapid7/pdf-renderer/web"
)

type ResponseSummary struct {
	Url string `json:"url"`
	Status int `json:"status"`
	StatusText string `json:"statusText"`
}

const DEFAULT_REQUEST_POLL_RETRIES = 10
const DEFAULT_REQUEST_POLL_INTERVAL = "1s"
const DEFAULT_PRINT_DEADLINE = "5m"

func requestPollRetries() int {
	requestPollRetries := DEFAULT_REQUEST_POLL_RETRIES
	configRequestPollRetries := os.Getenv("PDF_RENDERER_REQUEST_POLL_RETRIES")
	if len(configRequestPollRetries) > 0 {
		tmp, err := strconv.Atoi(configRequestPollRetries)
		if err == nil {
			requestPollRetries = tmp
		}
	}

	return requestPollRetries
}

func requestPollInterval() time.Duration {
	requestPollInterval, _ := time.ParseDuration(DEFAULT_REQUEST_POLL_INTERVAL)
	configRequestPollInterval := os.Getenv("PDF_RENDERER_REQUEST_POLL_INTERVAL")
	if len(configRequestPollInterval) > 0 {
		tmp, err := time.ParseDuration(configRequestPollInterval)
		if err == nil {
			requestPollInterval = tmp
		}
	}

	return requestPollInterval
}

func printDeadline() time.Duration {
	printDeadline, _ := time.ParseDuration(DEFAULT_PRINT_DEADLINE)
	configPrintDeadline := os.Getenv("PDF_RENDERER_PRINT_DEADLINE_MINUTES")
	if len(configPrintDeadline) > 0 {
		tmp, err := time.ParseDuration(configPrintDeadline)
		if err == nil {
			printDeadline = tmp
		}
	}

	return printDeadline
}

func listenForRequest(c chan *network.RequestWillBeSentReply, requestWillBeSentClient network.RequestWillBeSentClient) {
	defer func() {recover()}()

	for {
		reply, _ := requestWillBeSentClient.Recv()
		select {
		case c <- reply:
		default:
		}
	}
}

func listenForResponse(c chan *network.ResponseReceivedReply, responseReceivedClient network.ResponseReceivedClient) {
	defer func() {recover()}()

	for {
		reply, _ := responseReceivedClient.Recv()
		select {
		case c <- reply:
		default:
		}
	}
}

func CreatePdf(ctx context.Context, request web.GeneratePdfRequest) ([]byte, []byte, error) {
	// Use the DevTools API to manage targets
	devt := devtool.New("http://127.0.0.1:9222")
	pt, err := devt.Get(ctx, devtool.Page)
	if err != nil {
		pt, err = devt.Create(ctx)
		if err != nil {
			return nil, nil, err
		}
	}
	defer devt.Close(ctx, pt)

	// Open a new RPC connection to the Chrome Debugging Protocol target
	conn, err := rpcc.DialContext(ctx, pt.WebSocketDebuggerURL)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()

	// Create new browser context
	baseBrowser := cdp.NewClient(conn)

	// Initialize session manager for connecting to targets.
	sessionManager, err := session.NewManager(baseBrowser)
	if err != nil {
		return nil, nil, err
	}
	defer sessionManager.Close()

	// Basically create an incognito window
	newContextTarget, err := baseBrowser.Target.CreateBrowserContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer baseBrowser.Target.DisposeBrowserContext(ctx, target.NewDisposeBrowserContextArgs(newContextTarget.BrowserContextID))

	// Create a new blank target
	newTargetArgs := target.NewCreateTargetArgs("about:blank").SetBrowserContextID(newContextTarget.BrowserContextID)
	newTarget, err := baseBrowser.Target.CreateTarget(ctx, newTargetArgs)
	if err != nil {
		return nil, nil, err
	}
	closeTargetArgs := target.NewCloseTargetArgs(newTarget.TargetID)
	defer func() {
		closeReply, err := baseBrowser.Target.CloseTarget(ctx, closeTargetArgs)
		if err != nil || !closeReply.Success {
			log.Error(fmt.Sprintf("Could not close target for: %v because: %v", request.TargetUrl, err))
		}
	}()

	// Connect to target using the existing websocket connection.
	newContextConn, err := sessionManager.Dial(ctx, newTarget.TargetID)
	if err != nil {
		return nil, nil, err
	}
	defer newContextConn.Close()
	c := cdp.NewClient(newContextConn)

	// Enable the runtime
	err = c.Runtime.Enable(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Enable the network
	err = c.Network.Enable(ctx, network.NewEnableArgs())
	if err != nil {
		return nil, nil, err
	}

	// Set custom headers
	if request.Headers != nil {
		headers, marshallErr := json.Marshal(request.Headers)
		if marshallErr != nil {
			return nil, nil, marshallErr
		}
		extraHeaders := network.NewSetExtraHTTPHeadersArgs(headers)

		err = c.Network.SetExtraHTTPHeaders(ctx, extraHeaders)
		if err != nil {
			return nil, nil, err
		}
	}

	// Enable events
	err = c.Page.Enable(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Start listening for requests
	requestWillBeSentClient, _ := c.Network.RequestWillBeSent(ctx)
	defer requestWillBeSentClient.Close()

	responseReceivedClient, _ := c.Network.ResponseReceived(ctx)
	defer responseReceivedClient.Close()

	requestWillBeSentChan := make(chan *network.RequestWillBeSentReply, 64)
	defer close(requestWillBeSentChan)

	responseReceivedChan := make(chan *network.ResponseReceivedReply, 64)
	defer close(responseReceivedChan)

	go listenForRequest(requestWillBeSentChan, requestWillBeSentClient)
	go listenForResponse(responseReceivedChan, responseReceivedClient)

	// Tell the page to navigate to the URL
	navArgs := page.NewNavigateArgs(request.TargetUrl)
	c.Page.Navigate(ctx, navArgs)

	// Wait for the page to finish loading
	var responseSummaries []ResponseSummary
	curAttempt := 0
	pendingRequests := 0
	requestPollRetries := requestPollRetries()
	requestPollInterval := requestPollInterval()
	printDeadline := printDeadline()
	startTime := time.Now()
	for time.Since(startTime) < printDeadline && curAttempt < requestPollRetries {
		time.Sleep(requestPollInterval)

		ConsumeChannels:
		for {
			select {
			case reply := <-requestWillBeSentChan:
				if nil == reply {
					break
				}

				if reply.Type.String() != "Document" {
					log.Debug(fmt.Sprintf("Requested: %v", reply.Request.URL))
					pendingRequests++
					curAttempt = 0
				}
				break
			case reply := <-responseReceivedChan:
				if nil == reply {
					break
				}

				if reply.Type.String() != "Document" {
					summary := ResponseSummary{
						Url: reply.Response.URL,
						Status: reply.Response.Status,
						StatusText: reply.Response.StatusText,
					}
					responseSummaries = append(responseSummaries, summary)
					if reply.Response.Status >= 400 {
						log.Error(fmt.Sprintf("Status: %v, Received: %v", reply.Response.Status, reply.Response.URL))
					} else {
						log.Debug(fmt.Sprintf("Status: %v, Received: %v", reply.Response.Status, reply.Response.URL))
					}
					pendingRequests--
				}
				break
			default:
				break ConsumeChannels
			}
		}

		if pendingRequests <= 0 {
			curAttempt++
		}
	}

	// Print to PDF
	printToPDFArgs := page.NewPrintToPDFArgs().
		SetLandscape(request.Orientation == "Landscape").
		SetPrintBackground(request.PrintBackground).
		SetMarginTop(request.MarginTop).
		SetMarginRight(request.MarginRight).
		SetMarginBottom(request.MarginBottom).
		SetMarginLeft(request.MarginLeft)
	pdf, err := c.Page.PrintToPDF(ctx, printToPDFArgs)
	if err != nil {
		return nil, nil, err
	}

	summaries, _ := json.Marshal(responseSummaries)

	return summaries, pdf.Data, nil
}
