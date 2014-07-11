package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/bmizerany/pat"
	"github.com/r3b/goku"
	"github.com/r3b/usergrid-go-sdk"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	// "strings"
	"syscall"
	"text/template"
	"time"
)

type QueueItem struct {
	email string
	site  url.URL
}
type EmailUser struct {
	Username    string
	Password    string
	EmailServer string
	Port        int
}
type SmtpTemplateData struct {
	From    string
	To      string
	Subject string
	Site    string
}

const EMAILTEMPLATE = `From: {{.From}}
To: {{.To}}
Subject: {{.Subject}}

Your request for {{.Site}} has been processed. Check it out the game at http://fasterness.com/game and nice, legible charts at http://fasterness.com/speedreport.

Thanks for playing!

-ryan`

const EMAILTEMPLATEEXIT = `From: {{.From}}
To: {{.To}}
Subject: {{.Subject}}

You broke something.

-ryan`

var (
	CLIENT_ID      string            = os.Getenv("UG_CLIENT_ID")
	CLIENT_SECRET  string            = os.Getenv("UG_CLIENT_SECRET")
	EMAIL_USER     string            = os.Getenv("SR_EMAIL_USER")
	EMAIL_PASSWORD string            = os.Getenv("SR_EMAIL_PASSWORD")
	HOST           string            = os.Getenv("HOST")
	PORT           string            = os.Getenv("PORT")
	PHANTOMJS      string            = os.Getenv("PHANTOMJS")
	SPEEDREPORT    string            = os.Getenv("SR_SCRIPT")
	queue          *goku.Queue       = goku.New(1000)
	m0             *runtime.MemStats = &runtime.MemStats{}
	m1             *runtime.MemStats = &runtime.MemStats{}
	client         usergrid.Client   = usergrid.Client{Organization: "rbridges", Application: "speedreport", Uri: "http://api.usergrid.com"}
	emailUser      *EmailUser        = &EmailUser{
		EMAIL_USER,
		EMAIL_PASSWORD,
		"smtp.1and1.com",
		587,
	}
	bodyTemplate *template.Template
	auth         smtp.Auth
	t0           time.Time
)

func onExit() {
	defer os.Exit(1)
	log.Printf("Sending exit notifier.")
	context := &SmtpTemplateData{
		"speedreport@fasterness.com",
		"ryan@fasterness.com",
		"The server has exited.",
		"",
	}
	var doc bytes.Buffer
	err := bodyTemplate.Execute(&doc, context)
	if err != nil {
		log.Print("error trying to execute mail template")
	}
	err = smtp.SendMail(emailUser.EmailServer+":"+strconv.Itoa(emailUser.Port), // in our case, "smtp.google.com:587"
		auth,
		"speedreport@fasterness.com",
		[]string{"ryan@fasterness.com"},
		doc.Bytes())
	if err != nil {
		log.Print("ERROR: attempting to send a mail ", err)
	}
	stat("Exiting...")
}
func trap() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, os.Kill)
	signal.Notify(sig, syscall.SIGTERM)
	signal.Notify(sig, syscall.SIGABRT)
	stat("Starting up")
	for {
		s := <-sig
		log.Printf("Got signal: %n", s)
		log.Printf("Current queue size: %d\n", queue.Size())
		stat("Exiting...")
		os.Exit(0)
	}
}
func stat(message string) {
	log.Printf(message)
	runtime.ReadMemStats(m1)
	num := runtime.NumGoroutine()
	log.Printf("Number of goroutines: %d\n", num)
	log.Printf("Current total memory: %.2fmb\n", float64(m1.Sys)/(1024*1024))
	log.Printf("Per goroutine:\n")
	log.Printf("\tMemory: %.2f bytes\n", float64(m1.Sys-m0.Sys)/float64(num))
	log.Printf("\tTime:   %.2f µs\n", float64(time.Since(t0).Nanoseconds())/float64(num)/1e3)
}
func statJSON() (string, error) {
	runtime.ReadMemStats(m1)
	num := runtime.NumGoroutine()
	dur := time.Since(t0)
	things := map[string]interface{}{
		"goroutines":     num,
		"total_memory":   float64(m1.Sys) / (1024 * 1024),
		"uptime":         float64(dur.Seconds()),
		"uptime_hours":   dur.Hours(),
		"uptime_minutes": dur.Minutes(),
		"uptime_seconds": dur.Seconds(),
		"queue_depth":    queue.Size(),
	}

	body, err := json.Marshal(things)
	return string(body), err
}
func init() {
	go trap()
	if CLIENT_ID == "" || CLIENT_SECRET == "" {
		log.Fatalf("Usergrid authentication information not found.")
	}
	if EMAIL_USER == "" || EMAIL_PASSWORD == "" {
		log.Fatalf("Email authentication information not found.")
	}
	err := client.OrgLogin(CLIENT_ID, CLIENT_SECRET)
	if err != nil {
		log.Fatalf(err.Error())
	}
	if PHANTOMJS == "" {
		PHANTOMJS = "phantomjs"
	}
	if SPEEDREPORT == "" {
		SPEEDREPORT = "/home/ubuntu/speedreport/speedreport.js"
	}
	auth = smtp.PlainAuth("", emailUser.Username, emailUser.Password, emailUser.EmailServer)
	t := template.New("EMAILTEMPLATE")
	bodyTemplate, err = t.Parse(EMAILTEMPLATE)
	if err != nil {
		log.Panicf("error trying to parse mail template")
	}
	t0 = time.Now().UTC()

}
func main() {
	defer onExit()
	waiting, awaiting_notification, completed := make(chan *QueueItem), make(chan *QueueItem), make(chan *QueueItem)

	go QueueReader(waiting)
	go TestRunner(waiting, awaiting_notification)
	go Notifier(awaiting_notification, completed)

	mux := pat.New()
	mux.Get("/test", http.HandlerFunc(TestHandler))
	mux.Get("/status", http.HandlerFunc(StatusHandler))
	mux.Options("/test", http.HandlerFunc(OptionsHandler))
	mux.Options("/status", http.HandlerFunc(OptionsHandler))
	http.Handle("/", mux)

	stat("Server is ready.")
	if HOST == "" {
		HOST = "127.0.0.1"
	}
	if PORT == "" {
		PORT = "80"
	}
	http.ListenAndServe(fmt.Sprintf("%s:%s", HOST, PORT), nil)
}

func AlreadyInQueue(queue goku.Queue, address url.URL) bool {
	for i := range queue.Items {
		item := queue.GetItem(i).(QueueItem)
		if address.String() == item.site.String() {
			return true
		}
	}
	return false
}
func SaveRequest(request QueueItem) {
	var objmap interface{}
	data := map[string]string{
		"url":   request.site.String(),
		"email": request.email,
	}
	log.Printf("Saving request: %s\n", data)
	err := client.Post("requests", nil, data, usergrid.JSONResponseHandler(&objmap))
	if err != nil {
		log.Printf("Error saving request: %v\n", err)
		// } else {
		// 	log.Printf("Saved request: %v\n", objmap)
	}
}
func QueueReader(out chan<- *QueueItem) {
	for {
		if queue.IsEmpty() {
			time.Sleep(30 * time.Second)
		} else {
			item := queue.Dequeue()
			qitem := item.(QueueItem)
			go SaveRequest(qitem)
			log.Printf("Request for %v", qitem)
			out <- &qitem
		}
	}
}

func TestRunner(in <-chan *QueueItem, out chan<- *QueueItem) {
	for {
		select {
		case v := <-in:
			//run program
			go func() {
				log.Printf("Testing %v", v)
				cmd := exec.Command(PHANTOMJS, SPEEDREPORT, "--url="+v.site.String())
				var output bytes.Buffer
				cmd.Stdout = &output
				if err := cmd.Run(); err != nil {
					log.Println(err)
				}
				// v.uuid = strings.TrimSpace(output.String())
				// log.Println(v.uuid)
				out <- v
			}()
		default:
			time.Sleep(5 * time.Second)
		}

	}
}
func Notifier(in <-chan *QueueItem, out chan<- *QueueItem) {
	for {
		select {
		case v := <-in:
			go func() {
				log.Printf("Notifying %v", v.email)

				context := &SmtpTemplateData{
					"speedreport@fasterness.com",
					v.email,
					"Your Web Performance adventure is waiting.",
					v.site.String(),
				}
				var doc bytes.Buffer
				err := bodyTemplate.Execute(&doc, context)
				if err != nil {
					log.Print("error trying to execute mail template")
				}
				err = smtp.SendMail(emailUser.EmailServer+":"+strconv.Itoa(emailUser.Port), // in our case, "smtp.google.com:587"
					auth,
					"speedreport@fasterness.com",
					[]string{v.email},
					doc.Bytes())
				if err != nil {
					log.Print("ERROR: attempting to send a mail ", err)
				} else {
					log.Printf("Notification to %v was sent successfully", v.email)
				}
				out <- v
			}()
		default:
			time.Sleep(5 * time.Second)
		}
	}
}
func OptionsHandler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Add("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	writer.Header().Add("Access-Control-Allow-Headers", "Content-Type")
	writer.Header().Add("Access-Control-Allow-Origin", "*")
	writer.Header().Add("Content-Type", "application/json")
	writer.Write([]byte(""))
}
func StatusHandler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Add("Access-Control-Allow-Origin", "*")
	writer.Header().Add("Content-Type", "application/json")
	status, _ := statJSON()
	writer.Write([]byte(status))
}
func TestHandler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Add("Access-Control-Allow-Origin", "*")
	writer.Header().Add("Content-Type", "application/json")
	params := request.URL.Query()
	testUrl := params.Get(":url")
	if testUrl == "" {
		testUrl = params.Get("url")
	}

	email := params.Get(":email")
	if email == "" {
		email = params.Get("email")
	}

	if testUrl == "" {
		http.Error(writer, "no URL specified", http.StatusNotAcceptable)
	} else {

		testUrl, _ := url.QueryUnescape(testUrl)
		matched, err := regexp.MatchString("/^https?://", testUrl)
		if !matched {
			testUrl = "http://" + testUrl
		}
		address, err := url.Parse(testUrl)
		if err != nil || address.Host == "" {
			http.Error(writer, fmt.Sprintf("Could not parse URL: %s", testUrl), http.StatusBadRequest)
			return
		}
		if address.Scheme == "" {
			address.Scheme = "http"
		}
		if AlreadyInQueue(*queue, *address) {
			writer.Write([]byte(fmt.Sprintf("%s is already in the queue", testUrl)))
			return
		}
		email, _ := url.QueryUnescape(email)
		item := QueueItem{site: *address, email: email}
		queue.Enqueue(item)
		// waiting <- item
		response := "Testing " + testUrl
		if email != "" {
			response += "\nWe will email the results to " + email
		}
		response += fmt.Sprintf("\nNumber %d in queue", queue.Size())
		writer.Write([]byte(response))
	}
}
