package attach

import (
	"github.com/mholt/caddy"
	"github.com/mholt/caddy/caddyhttp/httpserver"
	"github.com/cwarner818/giota"
	"github.com/pkg/errors"
	"net/http"
	"encoding/json"
	"fmt"
	"time"
	"io/ioutil"
	"bytes"
)

var ErrMissingBody = errors.New("missing body")
var ErrBodyUnparsable = errors.New("body can't be parsed to command")
var ErrFetchRawTips = errors.New("couldn't fetch raw tips data from node")
var ErrBuildingTx = errors.New("couldn't build transaction from trytes")
var ErrBuildingRes = errors.New("couldn't build response")

func init() {
	caddy.RegisterPlugin("attach", caddy.Plugin{
		ServerType: "http",
		Action:     setup,
	})
}

var powFn giota.PowFunc

func setup(c *caddy.Controller) error {
	name, powfunc := giota.GetBestPoW()
	powFn = powfunc
	fmt.Println("using proof of work:", name)
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

func (h AttachToTangleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {
	if r.Method != http.MethodPost {
		return h.Next.ServeHTTP(w, r)
	}

	if r.Body == nil {
		return http.StatusBadRequest, ErrMissingBody
	}

	contents, err := ioutil.ReadAll(r.Body)
	if err != nil {
		panic(err)
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

	start := time.Now().UnixNano()

	trunkTxHash := command.TrunkTxHash
	branchTxHash := command.BranchTxHash
	txTrytes := command.Trytes

	if len(txTrytes) == 0 {
		return h.Next.ServeHTTP(w, r)
	}

	transactions := []giota.Transaction{}
	for i := len(txTrytes) - 1; i >= 0; i--{
		tx, err := giota.NewTransaction(txTrytes[i])
		if err != nil {
			return http.StatusBadRequest, ErrBuildingTx
		}
		transactions = append(transactions, *tx)

	}

	bundle := &Transaction{
		Trunk:        trunkTxHash,
		Branch:       branchTxHash,
		Transactions: transactions,
	}

	fmt.Printf("doing pow for %d txs\n", len(transactions))
	s := time.Now().UnixNano()
	doPow(bundle, 3, bundle.Transactions, 14, powFn)
	fmt.Printf("took %dms to do pow for %d txs", (time.Now().UnixNano() - s) / 1000000, len(transactions))

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
	w.Header().Set("access-control-allow-origin","*")
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

func doPow(tra *Transaction, depth int64, tx []giota.Transaction, mwm int64, pow giota.PowFunc) error {
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
