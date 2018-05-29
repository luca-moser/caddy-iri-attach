package attach

import (
	"github.com/mholt/caddy"
	"github.com/mholt/caddy/caddyhttp/httpserver"
	"github.com/cwarner818/giota"
	"github.com/pkg/errors"
	"net/http"
	"encoding/json"
	"time"
	"io/ioutil"
	"log"
	"bytes"
	"sync"
	"strconv"
	"math"
)

var ErrMissingBody = errors.New("missing body")
var ErrBodyUnparsable = errors.New("body can't be parsed to command")
var ErrFetchRawTips = errors.New("couldn't fetch raw tips data from node")
var ErrBuildingTx = errors.New("couldn't build transaction from trytes")
var ErrBuildingRes = errors.New("couldn't build response")
var ErrMissingTxBundleLimit = errors.New("expected tx bundle limit after the attach directive")
var ErrTxBundleLimitExceeded = errors.New("the number of transactions exceeds the limit")

func init() {
	caddy.RegisterPlugin("attach", caddy.Plugin{
		ServerType: "http",
		Action:     setup,
	})
}

var powFn giota.PowFunc
var maxTxInBundle = 200

func setup(c *caddy.Controller) error {
	name, powfunc := giota.GetBestPoW()
	powFn = powfunc
	var err error
	for c.Next() {
		if !c.NextArg() {
			break
		}
		maxTxInBundle, err = strconv.Atoi(c.Val())
		if err != nil {
			log.Printf("setting default max bundle txs to %d\n", 200)
			maxTxInBundle = 200
			continue
		}
	}
	log.Printf("attachToTangle interception configured with max bundle txs limit of %d\n", maxTxInBundle)
	log.Printf("using proof of work method: %s\n", name)
	cfg := httpserver.GetConfig(c)
	mid := func(next httpserver.Handler) httpserver.Handler {
		return AttachToTangleHandler{Next: next}
	}
	cfg.AddMiddleware(mid)
	return nil
}

type AttachToTangleHandler struct {
	Next httpserver.Handler
}

type AttachToTangleCmd struct {
	Command      string         `json:"command"`
	TrunkTxHash  giota.Trytes   `json:"trunkTransaction"`
	BranchTxHash giota.Trytes   `json:"branchTransaction"`
	MWM          int            `json:"minWeightMagnitude"`
	Trytes       []giota.Trytes `json:"trytes"`
}

type AttachToTangleRes struct {
	Trytes   []giota.Trytes `json:"trytes"`
	Duration int64          `json:"duration"`
}

const attachToTangleCommand = "attachToTangle"

var mu = sync.Mutex{}

func (h AttachToTangleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {
	if r.Method != http.MethodPost {
		return h.Next.ServeHTTP(w, r)
	}

	if r.Body == nil {
		return http.StatusBadRequest, ErrMissingBody
	}

	contents, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return http.StatusBadRequest, ErrMissingBody
	}

	command := &AttachToTangleCmd{}
	err = json.NewDecoder(bytes.NewReader(contents)).Decode(&command);
	// re-add body
	r.Body = ioutil.NopCloser(bytes.NewReader(contents))
	if err != nil {
		// instead of aborting, send it further to IRI
		return h.Next.ServeHTTP(w, r)
	}

	// only intercept attachToTangle command
	if command.Command != attachToTangleCommand {
		return h.Next.ServeHTTP(w, r)
	}

	// only allow one PoW at a time
	// we could lock later but for keeping log order we do it from here
	mu.Lock()
	defer mu.Unlock()

	log.Printf("new attachToTangle request from %s\n", r.RemoteAddr)
	start := time.Now().UnixNano()

	trunkTxHash := command.TrunkTxHash
	branchTxHash := command.BranchTxHash
	txTrytes := command.Trytes

	if len(txTrytes) == 0 {
		return h.Next.ServeHTTP(w, r)
	}

	if len(txTrytes) > maxTxInBundle {
		return http.StatusBadRequest, errors.Wrapf(ErrTxBundleLimitExceeded, "max allowed is %d", maxTxInBundle)
	}

	var isValueTransaction bool
	var inputValue int64
	transactions := []giota.Transaction{}
	for i := len(txTrytes) - 1; i >= 0; i-- {
		tx, err := giota.NewTransaction(txTrytes[i])
		if err != nil {
			return http.StatusBadRequest, ErrBuildingTx
		}
		if tx.Value > 0 {
			isValueTransaction = true
		}
		if tx.Value < 0 {
			inputValue += tx.Value
		}
		transactions = append(transactions, *tx)
	}

	if isValueTransaction {
		log.Printf("bundle is using %d IOTAs as input\n", int64(math.Abs(float64(inputValue))))
	}

	bundle := &Transaction{
		Trunk:        trunkTxHash,
		Branch:       branchTxHash,
		Transactions: transactions,
	}

	log.Printf("doing pow for bundle with %d txs (value tx=%v)\n", len(transactions), isValueTransaction)
	s := time.Now().UnixNano()
	doPow(bundle, bundle.Transactions, 14, powFn)
	log.Printf("took %dms to do pow for bundle with %d txs\n", (time.Now().UnixNano()-s)/1000000, len(transactions))

	// construct response
	trytesRes := []giota.Trytes{}
	for _, tx := range bundle.Transactions {
		trytesRes = append(trytesRes, tx.Trytes())
	}

	res := &AttachToTangleRes{Trytes: trytesRes, Duration: (time.Now().UnixNano() - start) / 1000000}

	resBytes, err := json.Marshal(res)
	if err != nil {
		return http.StatusInternalServerError, ErrBuildingRes
	}

	w.Header().Set(contentType, contentTypeJSON)
	w.Header().Set("access-control-allow-origin", "*")
	w.Write(resBytes)
	return http.StatusOK, nil
}

const (
	contentType        = "Content-Type"
	contentTypeJSON    = "application/json"
	maxTimestampTrytes = "L99999999"
)

type Tips struct {
	Trunk, Branch         giota.Transaction
	TrunkHash, BranchHash giota.Trytes
}

type Transaction struct {
	Trunk, Branch giota.Trytes
	Transactions  []giota.Transaction
}

func doPow(tra *Transaction, tx []giota.Transaction, mwm int64, pow giota.PowFunc) error {
	var prev giota.Trytes
	var err error
	for i := len(tx) - 1; i >= 0; i-- {
		switch {
		case i == len(tx)-1:
			tx[i].TrunkTransaction = tra.Trunk
			tx[i].BranchTransaction = tra.Branch
		default:
			tx[i].TrunkTransaction = prev
			tx[i].BranchTransaction = tra.Trunk
		}

		timestamp := giota.Int2Trits(time.Now().UnixNano()/1000000, giota.TimestampTrinarySize).Trytes()
		tx[i].AttachmentTimestamp = timestamp
		tx[i].AttachmentTimestampLowerBound = ""
		tx[i].AttachmentTimestampUpperBound = maxTimestampTrytes
		tx[i].Nonce, err = pow(tx[i].Trytes(), int(mwm))

		if err != nil {
			return err
		}

		prev = tx[i].Hash()
	}
	return nil
}
