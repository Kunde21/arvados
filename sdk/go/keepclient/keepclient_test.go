package keepclient

import (
	"crypto/md5"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Kunde21/arvados/sdk/go/arvadosclient"
	"github.com/Kunde21/arvados/sdk/go/arvadostest"
	"github.com/Kunde21/arvados/sdk/go/streamer"
	. "gopkg.in/check.v1"
)

// Gocheck boilerplate
func Test(t *testing.T) {
	TestingT(t)
}

// Gocheck boilerplate
var _ = Suite(&ServerRequiredSuite{})
var _ = Suite(&StandaloneSuite{})

var no_server = flag.Bool("no-server", false, "Skip 'ServerRequireSuite'")

// Tests that require the Keep server running
type ServerRequiredSuite struct{}

// Standalone tests
type StandaloneSuite struct{}

func pythonDir() string {
	cwd, _ := os.Getwd()
	return fmt.Sprintf("%s/../../python/tests", cwd)
}

func (s *ServerRequiredSuite) SetUpSuite(c *C) {
	if *no_server {
		c.Skip("Skipping tests that require server")
		return
	}
	arvadostest.StartAPI()
	arvadostest.StartKeep(2, false)
}

func (s *ServerRequiredSuite) TearDownSuite(c *C) {
	if *no_server {
		return
	}
	arvadostest.StopKeep(2)
	arvadostest.StopAPI()
}

func (s *ServerRequiredSuite) TestMakeKeepClient(c *C) {
	arv, err := arvadosclient.MakeArvadosClient()
	c.Assert(err, Equals, nil)

	kc, err := MakeKeepClient(&arv)

	c.Assert(err, Equals, nil)
	c.Check(len(kc.LocalRoots()), Equals, 2)
	for _, root := range kc.LocalRoots() {
		c.Check(root, Matches, "http://localhost:\\d+")
	}
}

func (s *ServerRequiredSuite) TestDefaultReplications(c *C) {
	arv, err := arvadosclient.MakeArvadosClient()
	c.Assert(err, Equals, nil)

	kc, err := MakeKeepClient(&arv)
	c.Assert(kc.Want_replicas, Equals, 2)

	arv.DiscoveryDoc["defaultCollectionReplication"] = 3.0
	kc, err = MakeKeepClient(&arv)
	c.Assert(kc.Want_replicas, Equals, 3)

	arv.DiscoveryDoc["defaultCollectionReplication"] = 1.0
	kc, err = MakeKeepClient(&arv)
	c.Assert(kc.Want_replicas, Equals, 1)
}

type StubPutHandler struct {
	c              *C
	expectPath     string
	expectApiToken string
	expectBody     string
	handled        chan string
}

func (sph StubPutHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	sph.c.Check(req.URL.Path, Equals, "/"+sph.expectPath)
	sph.c.Check(req.Header.Get("Authorization"), Equals, fmt.Sprintf("OAuth2 %s", sph.expectApiToken))
	body, err := ioutil.ReadAll(req.Body)
	sph.c.Check(err, Equals, nil)
	sph.c.Check(body, DeepEquals, []byte(sph.expectBody))
	resp.WriteHeader(200)
	sph.handled <- fmt.Sprintf("http://%s", req.Host)
}

func RunFakeKeepServer(st http.Handler) (ks KeepServer) {
	var err error
	ks.listener, err = net.ListenTCP("tcp", &net.TCPAddr{Port: 0})
	if err != nil {
		panic(fmt.Sprintf("Could not listen on any port"))
	}
	ks.url = fmt.Sprintf("http://%s", ks.listener.Addr().String())
	go http.Serve(ks.listener, st)
	return
}

func UploadToStubHelper(c *C, st http.Handler, f func(*KeepClient, string,
	io.ReadCloser, io.WriteCloser, chan uploadStatus)) {

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, _ := arvadosclient.MakeArvadosClient()
	arv.ApiToken = "abc123"

	kc, _ := MakeKeepClient(&arv)

	reader, writer := io.Pipe()
	upload_status := make(chan uploadStatus)

	f(kc, ks.url, reader, writer, upload_status)
}

func (s *StandaloneSuite) TestUploadToStubKeepServer(c *C) {
	log.Printf("TestUploadToStubKeepServer")

	st := StubPutHandler{
		c,
		"acbd18db4cc2f85cedef654fccc4a4d8",
		"abc123",
		"foo",
		make(chan string)}

	UploadToStubHelper(c, st,
		func(kc *KeepClient, url string, reader io.ReadCloser, writer io.WriteCloser, upload_status chan uploadStatus) {

			go kc.uploadToKeepServer(url, st.expectPath, reader, upload_status, int64(len("foo")), 0)

			writer.Write([]byte("foo"))
			writer.Close()

			<-st.handled
			status := <-upload_status
			c.Check(status, DeepEquals, uploadStatus{nil, fmt.Sprintf("%s/%s", url, st.expectPath), 200, 1, ""})
		})

	log.Printf("TestUploadToStubKeepServer done")
}

func (s *StandaloneSuite) TestUploadToStubKeepServerBufferReader(c *C) {
	log.Printf("TestUploadToStubKeepServerBufferReader")

	st := StubPutHandler{
		c,
		"acbd18db4cc2f85cedef654fccc4a4d8",
		"abc123",
		"foo",
		make(chan string)}

	UploadToStubHelper(c, st,
		func(kc *KeepClient, url string, reader io.ReadCloser,
			writer io.WriteCloser, upload_status chan uploadStatus) {

			tr := streamer.AsyncStreamFromReader(512, reader)
			defer tr.Close()

			br1 := tr.MakeStreamReader()

			go kc.uploadToKeepServer(url, st.expectPath, br1, upload_status, 3, 0)

			writer.Write([]byte("foo"))
			writer.Close()

			<-st.handled

			status := <-upload_status
			c.Check(status, DeepEquals, uploadStatus{nil, fmt.Sprintf("%s/%s", url, st.expectPath), 200, 1, ""})
		})

	log.Printf("TestUploadToStubKeepServerBufferReader done")
}

type FailHandler struct {
	handled chan string
}

func (fh FailHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	resp.WriteHeader(500)
	fh.handled <- fmt.Sprintf("http://%s", req.Host)
}

type FailThenSucceedHandler struct {
	handled        chan string
	count          int
	successhandler StubGetHandler
}

func (fh *FailThenSucceedHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	if fh.count == 0 {
		resp.WriteHeader(500)
		fh.count += 1
		fh.handled <- fmt.Sprintf("http://%s", req.Host)
	} else {
		fh.successhandler.ServeHTTP(resp, req)
	}
}

type Error404Handler struct {
	handled chan string
}

func (fh Error404Handler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	resp.WriteHeader(404)
	fh.handled <- fmt.Sprintf("http://%s", req.Host)
}

func (s *StandaloneSuite) TestFailedUploadToStubKeepServer(c *C) {
	log.Printf("TestFailedUploadToStubKeepServer")

	st := FailHandler{
		make(chan string)}

	hash := "acbd18db4cc2f85cedef654fccc4a4d8"

	UploadToStubHelper(c, st,
		func(kc *KeepClient, url string, reader io.ReadCloser,
			writer io.WriteCloser, upload_status chan uploadStatus) {

			go kc.uploadToKeepServer(url, hash, reader, upload_status, 3, 0)

			writer.Write([]byte("foo"))
			writer.Close()

			<-st.handled

			status := <-upload_status
			c.Check(status.url, Equals, fmt.Sprintf("%s/%s", url, hash))
			c.Check(status.statusCode, Equals, 500)
		})
	log.Printf("TestFailedUploadToStubKeepServer done")
}

type KeepServer struct {
	listener net.Listener
	url      string
}

func RunSomeFakeKeepServers(st http.Handler, n int) (ks []KeepServer) {
	ks = make([]KeepServer, n)

	for i := 0; i < n; i += 1 {
		ks[i] = RunFakeKeepServer(st)
	}

	return ks
}

func (s *StandaloneSuite) TestPutB(c *C) {
	log.Printf("TestPutB")

	hash := Md5String("foo")

	st := StubPutHandler{
		c,
		hash,
		"abc123",
		"foo",
		make(chan string, 5)}

	arv, _ := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)

	kc.Want_replicas = 2
	arv.ApiToken = "abc123"
	localRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	ks := RunSomeFakeKeepServers(st, 5)

	for i, k := range ks {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		defer k.listener.Close()
	}

	kc.SetServiceRoots(localRoots, writableLocalRoots, nil)

	kc.PutB([]byte("foo"))

	shuff := NewRootSorter(
		kc.LocalRoots(), Md5String("foo")).GetSortedRoots()

	s1 := <-st.handled
	s2 := <-st.handled
	c.Check((s1 == shuff[0] && s2 == shuff[1]) ||
		(s1 == shuff[1] && s2 == shuff[0]),
		Equals,
		true)

	log.Printf("TestPutB done")
}

func (s *StandaloneSuite) TestPutHR(c *C) {
	log.Printf("TestPutHR")

	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := StubPutHandler{
		c,
		hash,
		"abc123",
		"foo",
		make(chan string, 5)}

	arv, _ := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)

	kc.Want_replicas = 2
	arv.ApiToken = "abc123"
	localRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	ks := RunSomeFakeKeepServers(st, 5)

	for i, k := range ks {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		defer k.listener.Close()
	}

	kc.SetServiceRoots(localRoots, writableLocalRoots, nil)

	reader, writer := io.Pipe()

	go func() {
		writer.Write([]byte("foo"))
		writer.Close()
	}()

	kc.PutHR(hash, reader, 3)

	shuff := NewRootSorter(kc.LocalRoots(), hash).GetSortedRoots()
	log.Print(shuff)

	s1 := <-st.handled
	s2 := <-st.handled

	c.Check((s1 == shuff[0] && s2 == shuff[1]) ||
		(s1 == shuff[1] && s2 == shuff[0]),
		Equals,
		true)

	log.Printf("TestPutHR done")
}

func (s *StandaloneSuite) TestPutWithFail(c *C) {
	log.Printf("TestPutWithFail")

	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := StubPutHandler{
		c,
		hash,
		"abc123",
		"foo",
		make(chan string, 4)}

	fh := FailHandler{
		make(chan string, 1)}

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)

	kc.Want_replicas = 2
	arv.ApiToken = "abc123"
	localRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	ks1 := RunSomeFakeKeepServers(st, 4)
	ks2 := RunSomeFakeKeepServers(fh, 1)

	for i, k := range ks1 {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		defer k.listener.Close()
	}
	for i, k := range ks2 {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i+len(ks1))] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i+len(ks1))] = k.url
		defer k.listener.Close()
	}

	kc.SetServiceRoots(localRoots, writableLocalRoots, nil)

	shuff := NewRootSorter(
		kc.LocalRoots(), Md5String("foo")).GetSortedRoots()

	phash, replicas, err := kc.PutB([]byte("foo"))

	<-fh.handled

	c.Check(err, Equals, nil)
	c.Check(phash, Equals, "")
	c.Check(replicas, Equals, 2)

	s1 := <-st.handled
	s2 := <-st.handled

	c.Check((s1 == shuff[1] && s2 == shuff[2]) ||
		(s1 == shuff[2] && s2 == shuff[1]),
		Equals,
		true)
}

func (s *StandaloneSuite) TestPutWithTooManyFail(c *C) {
	log.Printf("TestPutWithTooManyFail")

	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := StubPutHandler{
		c,
		hash,
		"abc123",
		"foo",
		make(chan string, 1)}

	fh := FailHandler{
		make(chan string, 4)}

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)

	kc.Want_replicas = 2
	kc.Retries = 0
	arv.ApiToken = "abc123"
	localRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	ks1 := RunSomeFakeKeepServers(st, 1)
	ks2 := RunSomeFakeKeepServers(fh, 4)

	for i, k := range ks1 {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		defer k.listener.Close()
	}
	for i, k := range ks2 {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i+len(ks1))] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i+len(ks1))] = k.url
		defer k.listener.Close()
	}

	kc.SetServiceRoots(localRoots, writableLocalRoots, nil)

	_, replicas, err := kc.PutB([]byte("foo"))

	c.Check(err, Equals, InsufficientReplicasError)
	c.Check(replicas, Equals, 1)
	c.Check(<-st.handled, Equals, ks1[0].url)

	log.Printf("TestPutWithTooManyFail done")
}

type StubGetHandler struct {
	c              *C
	expectPath     string
	expectApiToken string
	httpStatus     int
	body           []byte
}

func (sgh StubGetHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	sgh.c.Check(req.URL.Path, Equals, "/"+sgh.expectPath)
	sgh.c.Check(req.Header.Get("Authorization"), Equals, fmt.Sprintf("OAuth2 %s", sgh.expectApiToken))
	resp.WriteHeader(sgh.httpStatus)
	resp.Header().Set("Content-Length", fmt.Sprintf("%d", len(sgh.body)))
	resp.Write(sgh.body)
}

func (s *StandaloneSuite) TestGet(c *C) {
	log.Printf("TestGet")

	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := StubGetHandler{
		c,
		hash,
		"abc123",
		http.StatusOK,
		[]byte("foo")}

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": ks.url}, nil, nil)

	r, n, url2, err := kc.Get(hash)
	defer r.Close()
	c.Check(err, Equals, nil)
	c.Check(n, Equals, int64(3))
	c.Check(url2, Equals, fmt.Sprintf("%s/%s", ks.url, hash))

	content, err2 := ioutil.ReadAll(r)
	c.Check(err2, Equals, nil)
	c.Check(content, DeepEquals, []byte("foo"))

	log.Printf("TestGet done")
}

func (s *StandaloneSuite) TestGet404(c *C) {
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := Error404Handler{make(chan string, 1)}

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": ks.url}, nil, nil)

	r, n, url2, err := kc.Get(hash)
	c.Check(err, Equals, BlockNotFound)
	c.Check(n, Equals, int64(0))
	c.Check(url2, Equals, "")
	c.Check(r, Equals, nil)
}

func (s *StandaloneSuite) TestGetFail(c *C) {
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := FailHandler{make(chan string, 1)}

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": ks.url}, nil, nil)
	kc.Retries = 0

	r, n, url2, err := kc.Get(hash)
	errNotFound, _ := err.(*ErrNotFound)
	c.Check(errNotFound, NotNil)
	c.Check(strings.Contains(errNotFound.Error(), "HTTP 500"), Equals, true)
	c.Check(errNotFound.Temporary(), Equals, true)
	c.Check(n, Equals, int64(0))
	c.Check(url2, Equals, "")
	c.Check(r, Equals, nil)
}

func (s *StandaloneSuite) TestGetFailRetry(c *C) {
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := &FailThenSucceedHandler{make(chan string, 1), 0,
		StubGetHandler{
			c,
			hash,
			"abc123",
			http.StatusOK,
			[]byte("foo")}}

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": ks.url}, nil, nil)

	r, n, url2, err := kc.Get(hash)
	defer r.Close()
	c.Check(err, Equals, nil)
	c.Check(n, Equals, int64(3))
	c.Check(url2, Equals, fmt.Sprintf("%s/%s", ks.url, hash))

	content, err2 := ioutil.ReadAll(r)
	c.Check(err2, Equals, nil)
	c.Check(content, DeepEquals, []byte("foo"))
}

func (s *StandaloneSuite) TestGetNetError(c *C) {
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": "http://localhost:62222"}, nil, nil)

	r, n, url2, err := kc.Get(hash)
	errNotFound, _ := err.(*ErrNotFound)
	c.Check(errNotFound, NotNil)
	c.Check(strings.Contains(errNotFound.Error(), "connection refused"), Equals, true)
	c.Check(errNotFound.Temporary(), Equals, true)
	c.Check(n, Equals, int64(0))
	c.Check(url2, Equals, "")
	c.Check(r, Equals, nil)
}

func (s *StandaloneSuite) TestGetWithServiceHint(c *C) {
	uuid := "zzzzz-bi6l4-123451234512345"
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	// This one shouldn't be used:
	ks0 := RunFakeKeepServer(StubGetHandler{
		c,
		"error if used",
		"abc123",
		http.StatusOK,
		[]byte("foo")})
	defer ks0.listener.Close()
	// This one should be used:
	ks := RunFakeKeepServer(StubGetHandler{
		c,
		hash + "+K@" + uuid,
		"abc123",
		http.StatusOK,
		[]byte("foo")})
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(
		map[string]string{"x": ks0.url},
		nil,
		map[string]string{uuid: ks.url})

	r, n, uri, err := kc.Get(hash + "+K@" + uuid)
	defer r.Close()
	c.Check(err, Equals, nil)
	c.Check(n, Equals, int64(3))
	c.Check(uri, Equals, fmt.Sprintf("%s/%s", ks.url, hash+"+K@"+uuid))

	content, err := ioutil.ReadAll(r)
	c.Check(err, Equals, nil)
	c.Check(content, DeepEquals, []byte("foo"))
}

// Use a service hint to fetch from a local disk service, overriding
// rendezvous probe order.
func (s *StandaloneSuite) TestGetWithLocalServiceHint(c *C) {
	uuid := "zzzzz-bi6l4-zzzzzzzzzzzzzzz"
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	// This one shouldn't be used, although it appears first in
	// rendezvous probe order:
	ks0 := RunFakeKeepServer(StubGetHandler{
		c,
		"error if used",
		"abc123",
		http.StatusOK,
		[]byte("foo")})
	defer ks0.listener.Close()
	// This one should be used:
	ks := RunFakeKeepServer(StubGetHandler{
		c,
		hash + "+K@" + uuid,
		"abc123",
		http.StatusOK,
		[]byte("foo")})
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(
		map[string]string{
			"zzzzz-bi6l4-yyyyyyyyyyyyyyy": ks0.url,
			"zzzzz-bi6l4-xxxxxxxxxxxxxxx": ks0.url,
			"zzzzz-bi6l4-wwwwwwwwwwwwwww": ks0.url,
			uuid: ks.url},
		nil,
		map[string]string{
			"zzzzz-bi6l4-yyyyyyyyyyyyyyy": ks0.url,
			"zzzzz-bi6l4-xxxxxxxxxxxxxxx": ks0.url,
			"zzzzz-bi6l4-wwwwwwwwwwwwwww": ks0.url,
			uuid: ks.url},
	)

	r, n, uri, err := kc.Get(hash + "+K@" + uuid)
	defer r.Close()
	c.Check(err, Equals, nil)
	c.Check(n, Equals, int64(3))
	c.Check(uri, Equals, fmt.Sprintf("%s/%s", ks.url, hash+"+K@"+uuid))

	content, err := ioutil.ReadAll(r)
	c.Check(err, Equals, nil)
	c.Check(content, DeepEquals, []byte("foo"))
}

func (s *StandaloneSuite) TestGetWithServiceHintFailoverToLocals(c *C) {
	uuid := "zzzzz-bi6l4-123451234512345"
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	ksLocal := RunFakeKeepServer(StubGetHandler{
		c,
		hash + "+K@" + uuid,
		"abc123",
		http.StatusOK,
		[]byte("foo")})
	defer ksLocal.listener.Close()
	ksGateway := RunFakeKeepServer(StubGetHandler{
		c,
		hash + "+K@" + uuid,
		"abc123",
		http.StatusInternalServerError,
		[]byte("Error")})
	defer ksGateway.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(
		map[string]string{"zzzzz-bi6l4-keepdisk0000000": ksLocal.url},
		nil,
		map[string]string{uuid: ksGateway.url})

	r, n, uri, err := kc.Get(hash + "+K@" + uuid)
	c.Assert(err, Equals, nil)
	defer r.Close()
	c.Check(n, Equals, int64(3))
	c.Check(uri, Equals, fmt.Sprintf("%s/%s", ksLocal.url, hash+"+K@"+uuid))

	content, err := ioutil.ReadAll(r)
	c.Check(err, Equals, nil)
	c.Check(content, DeepEquals, []byte("foo"))
}

type BarHandler struct {
	handled chan string
}

func (this BarHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	resp.Write([]byte("bar"))
	this.handled <- fmt.Sprintf("http://%s", req.Host)
}

func (s *StandaloneSuite) TestChecksum(c *C) {
	foohash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))
	barhash := fmt.Sprintf("%x", md5.Sum([]byte("bar")))

	st := BarHandler{make(chan string, 1)}

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": ks.url}, nil, nil)

	r, n, _, err := kc.Get(barhash)
	_, err = ioutil.ReadAll(r)
	c.Check(n, Equals, int64(3))
	c.Check(err, Equals, nil)

	<-st.handled

	r, n, _, err = kc.Get(foohash)
	_, err = ioutil.ReadAll(r)
	c.Check(n, Equals, int64(3))
	c.Check(err, Equals, BadChecksum)

	<-st.handled
}

func (s *StandaloneSuite) TestGetWithFailures(c *C) {
	content := []byte("waz")
	hash := fmt.Sprintf("%x", md5.Sum(content))

	fh := Error404Handler{
		make(chan string, 4)}

	st := StubGetHandler{
		c,
		hash,
		"abc123",
		http.StatusOK,
		content}

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	localRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	ks1 := RunSomeFakeKeepServers(st, 1)
	ks2 := RunSomeFakeKeepServers(fh, 4)

	for i, k := range ks1 {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		defer k.listener.Close()
	}
	for i, k := range ks2 {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i+len(ks1))] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i+len(ks1))] = k.url
		defer k.listener.Close()
	}

	kc.SetServiceRoots(localRoots, writableLocalRoots, nil)
	kc.Retries = 0

	// This test works only if one of the failing services is
	// attempted before the succeeding service. Otherwise,
	// <-fh.handled below will just hang! (Probe order depends on
	// the choice of block content "waz" and the UUIDs of the fake
	// servers, so we just tried different strings until we found
	// an example that passes this Assert.)
	c.Assert(NewRootSorter(localRoots, hash).GetSortedRoots()[0], Not(Equals), ks1[0].url)

	r, n, url2, err := kc.Get(hash)

	<-fh.handled
	c.Check(err, Equals, nil)
	c.Check(n, Equals, int64(3))
	c.Check(url2, Equals, fmt.Sprintf("%s/%s", ks1[0].url, hash))

	read_content, err2 := ioutil.ReadAll(r)
	c.Check(err2, Equals, nil)
	c.Check(read_content, DeepEquals, content)
}

func (s *ServerRequiredSuite) TestPutGetHead(c *C) {
	content := []byte("TestPutGetHead")

	arv, err := arvadosclient.MakeArvadosClient()
	kc, err := MakeKeepClient(&arv)
	c.Assert(err, Equals, nil)

	hash := fmt.Sprintf("%x", md5.Sum(content))

	{
		n, _, err := kc.Ask(hash)
		c.Check(err, Equals, BlockNotFound)
		c.Check(n, Equals, int64(0))
	}
	{
		hash2, replicas, err := kc.PutB(content)
		c.Check(hash2, Matches, fmt.Sprintf(`%s\+%d\b.*`, hash, len(content)))
		c.Check(replicas, Equals, 2)
		c.Check(err, Equals, nil)
	}
	{
		r, n, url2, err := kc.Get(hash)
		c.Check(err, Equals, nil)
		c.Check(n, Equals, int64(len(content)))
		c.Check(url2, Matches, fmt.Sprintf("http://localhost:\\d+/%s", hash))

		read_content, err2 := ioutil.ReadAll(r)
		c.Check(err2, Equals, nil)
		c.Check(read_content, DeepEquals, content)
	}
	{
		n, url2, err := kc.Ask(hash)
		c.Check(err, Equals, nil)
		c.Check(n, Equals, int64(len(content)))
		c.Check(url2, Matches, fmt.Sprintf("http://localhost:\\d+/%s", hash))
	}
}

type StubProxyHandler struct {
	handled chan string
}

func (this StubProxyHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	resp.Header().Set("X-Keep-Replicas-Stored", "2")
	this.handled <- fmt.Sprintf("http://%s", req.Host)
}

func (s *StandaloneSuite) TestPutProxy(c *C) {
	log.Printf("TestPutProxy")

	st := StubProxyHandler{make(chan string, 1)}

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)

	kc.Want_replicas = 2
	kc.Using_proxy = true
	arv.ApiToken = "abc123"
	localRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	ks1 := RunSomeFakeKeepServers(st, 1)

	for i, k := range ks1 {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		defer k.listener.Close()
	}

	kc.SetServiceRoots(localRoots, writableLocalRoots, nil)

	_, replicas, err := kc.PutB([]byte("foo"))
	<-st.handled

	c.Check(err, Equals, nil)
	c.Check(replicas, Equals, 2)

	log.Printf("TestPutProxy done")
}

func (s *StandaloneSuite) TestPutProxyInsufficientReplicas(c *C) {
	log.Printf("TestPutProxy")

	st := StubProxyHandler{make(chan string, 1)}

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)

	kc.Want_replicas = 3
	kc.Using_proxy = true
	arv.ApiToken = "abc123"
	localRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	ks1 := RunSomeFakeKeepServers(st, 1)

	for i, k := range ks1 {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		defer k.listener.Close()
	}
	kc.SetServiceRoots(localRoots, writableLocalRoots, nil)

	_, replicas, err := kc.PutB([]byte("foo"))
	<-st.handled

	c.Check(err, Equals, InsufficientReplicasError)
	c.Check(replicas, Equals, 2)

	log.Printf("TestPutProxy done")
}

func (s *StandaloneSuite) TestMakeLocator(c *C) {
	l, err := MakeLocator("91f372a266fe2bf2823cb8ec7fda31ce+3+Aabcde@12345678")
	c.Check(err, Equals, nil)
	c.Check(l.Hash, Equals, "91f372a266fe2bf2823cb8ec7fda31ce")
	c.Check(l.Size, Equals, 3)
	c.Check(l.Hints, DeepEquals, []string{"3", "Aabcde@12345678"})
}

func (s *StandaloneSuite) TestMakeLocatorNoHints(c *C) {
	l, err := MakeLocator("91f372a266fe2bf2823cb8ec7fda31ce")
	c.Check(err, Equals, nil)
	c.Check(l.Hash, Equals, "91f372a266fe2bf2823cb8ec7fda31ce")
	c.Check(l.Size, Equals, -1)
	c.Check(l.Hints, DeepEquals, []string{})
}

func (s *StandaloneSuite) TestMakeLocatorNoSizeHint(c *C) {
	l, err := MakeLocator("91f372a266fe2bf2823cb8ec7fda31ce+Aabcde@12345678")
	c.Check(err, Equals, nil)
	c.Check(l.Hash, Equals, "91f372a266fe2bf2823cb8ec7fda31ce")
	c.Check(l.Size, Equals, -1)
	c.Check(l.Hints, DeepEquals, []string{"Aabcde@12345678"})
}

func (s *StandaloneSuite) TestMakeLocatorPreservesUnrecognizedHints(c *C) {
	str := "91f372a266fe2bf2823cb8ec7fda31ce+3+Unknown+Kzzzzz+Afoobar"
	l, err := MakeLocator(str)
	c.Check(err, Equals, nil)
	c.Check(l.Hash, Equals, "91f372a266fe2bf2823cb8ec7fda31ce")
	c.Check(l.Size, Equals, 3)
	c.Check(l.Hints, DeepEquals, []string{"3", "Unknown", "Kzzzzz", "Afoobar"})
	c.Check(l.String(), Equals, str)
}

func (s *StandaloneSuite) TestMakeLocatorInvalidInput(c *C) {
	_, err := MakeLocator("91f372a266fe2bf2823cb8ec7fda31c")
	c.Check(err, Equals, InvalidLocatorError)
}

func (s *StandaloneSuite) TestPutBWant2ReplicasWithOnlyOneWritableLocalRoot(c *C) {
	hash := Md5String("foo")

	st := StubPutHandler{
		c,
		hash,
		"abc123",
		"foo",
		make(chan string, 5)}

	arv, _ := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)

	kc.Want_replicas = 2
	arv.ApiToken = "abc123"
	localRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	ks := RunSomeFakeKeepServers(st, 5)

	for i, k := range ks {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		if i == 0 {
			writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		}
		defer k.listener.Close()
	}

	kc.SetServiceRoots(localRoots, writableLocalRoots, nil)

	_, replicas, err := kc.PutB([]byte("foo"))

	c.Check(err, Equals, InsufficientReplicasError)
	c.Check(replicas, Equals, 1)

	c.Check(<-st.handled, Equals, localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", 0)])
}

func (s *StandaloneSuite) TestPutBWithNoWritableLocalRoots(c *C) {
	hash := Md5String("foo")

	st := StubPutHandler{
		c,
		hash,
		"abc123",
		"foo",
		make(chan string, 5)}

	arv, _ := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)

	kc.Want_replicas = 2
	arv.ApiToken = "abc123"
	localRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	ks := RunSomeFakeKeepServers(st, 5)

	for i, k := range ks {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		defer k.listener.Close()
	}

	kc.SetServiceRoots(localRoots, writableLocalRoots, nil)

	_, replicas, err := kc.PutB([]byte("foo"))

	c.Check(err, Equals, InsufficientReplicasError)
	c.Check(replicas, Equals, 0)
}

type StubGetIndexHandler struct {
	c              *C
	expectPath     string
	expectAPIToken string
	httpStatus     int
	body           []byte
}

func (h StubGetIndexHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	h.c.Check(req.URL.Path, Equals, h.expectPath)
	h.c.Check(req.Header.Get("Authorization"), Equals, fmt.Sprintf("OAuth2 %s", h.expectAPIToken))
	resp.WriteHeader(h.httpStatus)
	resp.Header().Set("Content-Length", fmt.Sprintf("%d", len(h.body)))
	resp.Write(h.body)
}

func (s *StandaloneSuite) TestGetIndexWithNoPrefix(c *C) {
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := StubGetIndexHandler{
		c,
		"/index",
		"abc123",
		http.StatusOK,
		[]byte(hash + "+3 1443559274\n\n")}

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": ks.url}, nil, nil)

	r, err := kc.GetIndex("x", "")
	c.Check(err, Equals, nil)

	content, err2 := ioutil.ReadAll(r)
	c.Check(err2, Equals, nil)
	c.Check(content, DeepEquals, st.body[0:len(st.body)-1])
}

func (s *StandaloneSuite) TestGetIndexWithPrefix(c *C) {
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := StubGetIndexHandler{
		c,
		"/index/" + hash[0:3],
		"abc123",
		http.StatusOK,
		[]byte(hash + "+3 1443559274\n\n")}

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": ks.url}, nil, nil)

	r, err := kc.GetIndex("x", hash[0:3])
	c.Check(err, Equals, nil)

	content, err2 := ioutil.ReadAll(r)
	c.Check(err2, Equals, nil)
	c.Check(content, DeepEquals, st.body[0:len(st.body)-1])
}

func (s *StandaloneSuite) TestGetIndexIncomplete(c *C) {
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := StubGetIndexHandler{
		c,
		"/index/" + hash[0:3],
		"abc123",
		http.StatusOK,
		[]byte(hash)}

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": ks.url}, nil, nil)

	_, err = kc.GetIndex("x", hash[0:3])
	c.Check(err, Equals, ErrIncompleteIndex)
}

func (s *StandaloneSuite) TestGetIndexWithNoSuchServer(c *C) {
	hash := fmt.Sprintf("%x", md5.Sum([]byte("foo")))

	st := StubGetIndexHandler{
		c,
		"/index/" + hash[0:3],
		"abc123",
		http.StatusOK,
		[]byte(hash)}

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": ks.url}, nil, nil)

	_, err = kc.GetIndex("y", hash[0:3])
	c.Check(err, Equals, ErrNoSuchKeepServer)
}

func (s *StandaloneSuite) TestGetIndexWithNoSuchPrefix(c *C) {
	st := StubGetIndexHandler{
		c,
		"/index/abcd",
		"abc123",
		http.StatusOK,
		[]byte("\n")}

	ks := RunFakeKeepServer(st)
	defer ks.listener.Close()

	arv, err := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)
	arv.ApiToken = "abc123"
	kc.SetServiceRoots(map[string]string{"x": ks.url}, nil, nil)

	r, err := kc.GetIndex("x", "abcd")
	c.Check(err, Equals, nil)

	content, err2 := ioutil.ReadAll(r)
	c.Check(err2, Equals, nil)
	c.Check(content, DeepEquals, st.body[0:len(st.body)-1])
}

type FailThenSucceedPutHandler struct {
	handled        chan string
	count          int
	successhandler StubPutHandler
}

func (h *FailThenSucceedPutHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	if h.count == 0 {
		resp.WriteHeader(500)
		h.count += 1
		h.handled <- fmt.Sprintf("http://%s", req.Host)
	} else {
		h.successhandler.ServeHTTP(resp, req)
	}
}

func (s *StandaloneSuite) TestPutBRetry(c *C) {
	st := &FailThenSucceedPutHandler{make(chan string, 1), 0,
		StubPutHandler{
			c,
			Md5String("foo"),
			"abc123",
			"foo",
			make(chan string, 5)}}

	arv, _ := arvadosclient.MakeArvadosClient()
	kc, _ := MakeKeepClient(&arv)

	kc.Want_replicas = 2
	arv.ApiToken = "abc123"
	localRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	ks := RunSomeFakeKeepServers(st, 2)

	for i, k := range ks {
		localRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		writableLocalRoots[fmt.Sprintf("zzzzz-bi6l4-fakefakefake%03d", i)] = k.url
		defer k.listener.Close()
	}

	kc.SetServiceRoots(localRoots, writableLocalRoots, nil)

	hash, replicas, err := kc.PutB([]byte("foo"))

	c.Check(err, Equals, nil)
	c.Check(hash, Equals, "")
	c.Check(replicas, Equals, 2)
}
