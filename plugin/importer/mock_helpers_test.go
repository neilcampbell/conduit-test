package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/common/models"
	"github.com/algorand/go-algorand-sdk/v2/encoding/msgpack"
	sdk "github.com/algorand/go-algorand-sdk/v2/types"

	"github.com/algorand/conduit/conduit"
	"github.com/algorand/conduit/conduit/plugins"
)

// mockAlgodServer represents a mock algod node with controllable behavior
type mockAlgodServer struct {
	server       *httptest.Server
	currentRound atomic.Uint64
	genesis      models.Genesis
	blocks       map[uint64]*models.BlockResponse
	deltas       map[uint64]*sdk.LedgerStateDelta

	// Instrumentation for testing
	mu                     sync.Mutex
	calls                  mockCalls
	setSyncRoundError      error
	waitForBlockAfterError error
	statusError            error
}

// mockCalls tracks all recorded API calls for testing
type mockCalls struct {
	SetSyncRound      []setSyncRoundCall
	Status            []statusCall
	WaitForBlockAfter []waitForBlockAfterCall
}

// setSyncRoundCall records a call to SetSyncRound
type setSyncRoundCall struct {
	Round uint64
}

// statusCall records a call to Status
type statusCall struct{}

// waitForBlockAfterCall records a call to WaitForBlockAfter
type waitForBlockAfterCall struct {
	Round uint64
}

// newMockAlgodServer creates a new mock algod server
func newMockAlgodServer(t *testing.T, initialRound uint64) *mockAlgodServer {
	t.Helper()

	mock := &mockAlgodServer{
		genesis: createTestGenesis(),
		blocks:  make(map[uint64]*models.BlockResponse),
		deltas:  make(map[uint64]*sdk.LedgerStateDelta),
	}
	mock.currentRound.Store(initialRound)

	// Create test blocks for rounds 0 to initialRound
	for i := uint64(0); i <= initialRound; i++ {
		mock.blocks[i] = createTestBlock(i)
		if i > 0 {
			mock.deltas[i] = createTestDelta(i)
		}
	}

	mux := http.NewServeMux()

	// Status endpoint
	mux.HandleFunc("/v2/status", func(w http.ResponseWriter, r *http.Request) {
		// Record the call and check for error
		mock.mu.Lock()
		mock.calls.Status = append(mock.calls.Status, statusCall{})
		statusErr := mock.statusError
		mock.mu.Unlock()

		if statusErr != nil {
			http.Error(w, statusErr.Error(), http.StatusInternalServerError)
			return
		}

		status := models.NodeStatus{
			LastRound:   mock.currentRound.Load(),
			LastVersion: "v1",
		}
		json.NewEncoder(w).Encode(status)
	})

	// Status after block endpoint
	mux.HandleFunc("/v2/status/wait-for-block-after/", func(w http.ResponseWriter, r *http.Request) {
		var afterRound uint64
		fmt.Sscanf(r.URL.Path, "/v2/status/wait-for-block-after/%d", &afterRound)

		// Record the call and check for error
		mock.mu.Lock()
		mock.calls.WaitForBlockAfter = append(mock.calls.WaitForBlockAfter, waitForBlockAfterCall{Round: afterRound})
		waitErr := mock.waitForBlockAfterError
		mock.mu.Unlock()

		if waitErr != nil {
			http.Error(w, waitErr.Error(), http.StatusInternalServerError)
			return
		}

		currentRound := mock.currentRound.Load()

		if afterRound < currentRound {
			status := models.NodeStatus{
				LastRound:   currentRound,
				LastVersion: "v1",
			}
			json.NewEncoder(w).Encode(status)
			return
		}

		status := models.NodeStatus{
			LastRound:   currentRound,
			LastVersion: "v1",
		}
		json.NewEncoder(w).Encode(status)
	})

	// Genesis endpoint
	mux.HandleFunc("/genesis", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mock.genesis)
	})

	// Block endpoint
	mux.HandleFunc("/v2/blocks/", func(w http.ResponseWriter, r *http.Request) {
		var round uint64
		fmt.Sscanf(r.URL.Path, "/v2/blocks/%d", &round)

		block, exists := mock.blocks[round]
		if !exists {
			http.Error(w, "block not found", http.StatusNotFound)
			return
		}

		blockBytes := msgpack.Encode(block)
		w.Header().Set("Content-Type", "application/msgpack")
		w.Write(blockBytes)
	})

	// Delta endpoint
	mux.HandleFunc("/v2/deltas/", func(w http.ResponseWriter, r *http.Request) {
		var round uint64
		fmt.Sscanf(r.URL.Path, "/v2/deltas/%d", &round)

		delta, exists := mock.deltas[round]
		if !exists {
			http.Error(w, "delta not found", http.StatusNotFound)
			return
		}

		deltaBytes := msgpack.Encode(delta)
		w.Header().Set("Content-Type", "application/msgpack")
		w.Write(deltaBytes)
	})

	// SetSyncRound endpoint
	mux.HandleFunc("/v2/ledger/sync/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the round number from the URL path
		var targetRound uint64
		fmt.Sscanf(r.URL.Path, "/v2/ledger/sync/%d", &targetRound)

		// Record the call and check for error
		mock.mu.Lock()
		mock.calls.SetSyncRound = append(mock.calls.SetSyncRound, setSyncRoundCall{
			Round: targetRound,
		})
		syncErr := mock.setSyncRoundError
		mock.mu.Unlock()

		if syncErr != nil {
			http.Error(w, syncErr.Error(), http.StatusInternalServerError)
			return
		}

		if targetRound > 0 {
			currentRound := mock.currentRound.Load()

			// Create blocks/deltas for any missing rounds
			for i := currentRound + 1; i <= targetRound; i++ {
				if _, exists := mock.blocks[i]; !exists {
					mock.blocks[i] = createTestBlock(i)
				}
				if i > 0 {
					if _, exists := mock.deltas[i]; !exists {
						mock.deltas[i] = createTestDelta(i)
					}
				}
			}

			// Advance the follower to the target round
			mock.currentRound.Store(targetRound)
		}

		w.WriteHeader(http.StatusOK)
	})

	mock.server = newIPv4HTTPServer(t, mux)
	return mock
}

// setRound sets the mock node to a specific round
func (m *mockAlgodServer) setRound(round uint64) {
	currentRound := m.currentRound.Load()
	for i := currentRound + 1; i <= round; i++ {
		m.blocks[i] = createTestBlock(i)
		if i > 0 {
			m.deltas[i] = createTestDelta(i)
		}
	}
	m.currentRound.Store(round)
}

// setWaitForBlockAfterError sets an error to be returned by the wait-for-block-after endpoint
func (m *mockAlgodServer) setWaitForBlockAfterError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waitForBlockAfterError = err
}

// setStatusError sets an error to be returned by the status endpoint
func (m *mockAlgodServer) setStatusError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusError = err
}

// close shuts down the mock server
func (m *mockAlgodServer) close() {
	m.server.Close()
}

// getCalls returns a copy of all recorded API calls
func (m *mockAlgodServer) getCalls() mockCalls {
	m.mu.Lock()
	defer m.mu.Unlock()
	return mockCalls{
		SetSyncRound:      append([]setSyncRoundCall{}, m.calls.SetSyncRound...),
		Status:            append([]statusCall{}, m.calls.Status...),
		WaitForBlockAfter: append([]waitForBlockAfterCall{}, m.calls.WaitForBlockAfter...),
	}
}

// clearCalls clears all recorded API call history
func (m *mockAlgodServer) clearCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = mockCalls{}
}

// createTestGenesis creates a test genesis block
func createTestGenesis() models.Genesis {
	return models.Genesis{
		Id:        "test-genesis-id",
		Network:   "testnet",
		Proto:     "future",
		Alloc:     []models.GenesisAllocation{},
		Rwd:       "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Fees:      "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		Timestamp: 1234567890,
		Comment:   "test genesis block",
		Devmode:   true,
	}
}

// createTestBlock creates a test block for a given round
func createTestBlock(round uint64) *models.BlockResponse {
	var genesisHash sdk.Digest
	copy(genesisHash[:], []byte{1, 2, 3, 4})

	return &models.BlockResponse{
		Block: sdk.Block{
			BlockHeader: sdk.BlockHeader{
				Round:       sdk.Round(round),
				GenesisID:   "test-genesis-id",
				GenesisHash: genesisHash,
				TimeStamp:   1234567890 + int64(round),
			},
			Payset: sdk.Payset{},
		},
		Cert: &map[string]interface{}{},
	}
}

// createTestDelta creates a test ledger state delta for a given round
func createTestDelta(round uint64) *sdk.LedgerStateDelta {
	return &sdk.LedgerStateDelta{
		Hdr: &sdk.BlockHeader{
			Round: sdk.Round(round),
		},
		Accts: sdk.AccountDeltas{
			Accts: []sdk.BalanceRecord{},
		},
	}
}

// createTestConfig creates a valid test configuration and returns it as a JSON string
func createTestConfig(leadURL, followerURL string) string {
	cfg := map[string]interface{}{
		"lead-node-url":          leadURL,
		"follower-node-url":      followerURL,
		"token":                  "test-token",
		"wait-for-round-timeout": "5s",
	}
	cfgBytes, _ := json.Marshal(cfg)
	return string(cfgBytes)
}

// createTestConfigWithTokens creates a test configuration with specific tokens
func createTestConfigWithTokens(leadURL, followerURL, token, leadToken, followerToken string) string {
	cfg := map[string]interface{}{
		"lead-node-url":          leadURL,
		"follower-node-url":      followerURL,
		"wait-for-round-timeout": "5s",
	}
	if token != "" {
		cfg["token"] = token
	}
	if leadToken != "" {
		cfg["lead-node-token"] = leadToken
	}
	if followerToken != "" {
		cfg["follower-node-token"] = followerToken
	}
	cfgBytes, _ := json.Marshal(cfg)
	return string(cfgBytes)
}

// requireMockServers creates and starts lead and follower mock servers
func requireMockServers(t *testing.T, leadRound, followerRound uint64) (lead *mockAlgodServer, follower *mockAlgodServer) {
	t.Helper()

	lead = newMockAlgodServer(t, leadRound)
	follower = newMockAlgodServer(t, followerRound)

	t.Cleanup(func() {
		lead.close()
		follower.close()
	})

	return lead, follower
}

// setupTestImporter creates an importer for testing
func setupTestImporter(t *testing.T, lead, follower *mockAlgodServer) *localnetImporter {
	t.Helper()

	cfgStr := createTestConfig(lead.server.URL, follower.server.URL)

	importer := &localnetImporter{}
	importer.leadRound.Store(lead.currentRound.Load())
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pipelineRound := sdk.Round(0)
	err := importer.Init(ctx, conduit.MakePipelineInitProvider(&pipelineRound, nil, nil), plugins.MakePluginConfig(cfgStr), logger)
	require.NoError(t, err)

	t.Cleanup(func() {
		importer.Close()
	})

	return importer
}

// newIPv4HTTPServer starts an httptest.Server bound to IPv4 loopback
func newIPv4HTTPServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create IPv4 listener: %v", err)
	}

	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: handler},
	}
	server.Start()
	t.Cleanup(server.Close)
	return server
}
