package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func TestMirrorDelivery_UnrelatedOwnershipIOKeepsDeliveredContent(t *testing.T) {
	for _, target := range []string{"agent:main:pico:direct:pico:kept-ownership", "sk_v1_new_kept_ownership"} {
		for _, fault := range []string{"witness directory", "metadata permission"} {
			t.Run(target+"/"+fault, func(t *testing.T) {
				if fault == "metadata permission" && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
					t.Skip("requires POSIX non-root permissions")
				}
				loop, instance, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
				defer cleanup()
				dir := t.TempDir()
				store, err := memory.NewJSONLStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				const other = "000_unrelated_canonical"
				if err = store.UpsertSessionMeta(t.Context(), other, nil, []string{"unrelated:alias"}); err != nil {
					t.Fatal(err)
				}
				if _, err = store.AppendHistoryReceipt(
					t.Context(),
					other,
					providers.Message{Role: "user", Content: "unrelated durable source"},
				); err != nil {
					t.Fatal(err)
				}
				metaPath := filepath.Join(dir, other+".meta.json")
				witnessPath := filepath.Join(dir, other+".jsonl.order-witness")
				restored := false
				restore := func() {
					if restored {
						return
					}
					if fault == "metadata permission" {
						if e := os.Chmod(metaPath, 0o644); e != nil {
							t.Fatal(e)
						}
					} else {
						if e := os.Remove(witnessPath); e != nil && !errors.Is(e, os.ErrNotExist) {
							t.Fatal(e)
						}
						if e := os.Rename(witnessPath+".fixture", witnessPath); e != nil {
							t.Fatal(e)
						}
					}
					restored = true
				}
				if fault == "metadata permission" {
					if err = os.Chmod(metaPath, 0); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(restore)
					if _, e := os.ReadFile(metaPath); !errors.Is(e, os.ErrPermission) {
						t.Fatal("fixture did not produce actual EACCES", e)
					}
				} else {
					if err = os.Rename(witnessPath, witnessPath+".fixture"); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(restore)
					if err = os.Mkdir(witnessPath, 0o700); err != nil {
						t.Fatal(err)
					}
					if _, e := os.ReadFile(witnessPath); e == nil {
						t.Fatal("fixture witness unexpectedly readable")
					}
				}
				store, err = memory.NewJSONLStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				instance.Sessions = session.NewJSONLBackend(store)
				loop.SetDeliverySessionResolver(func(channel, chatID string) string {
					if channel == "telegram" && chatID == "123" {
						return target
					}
					return ""
				})
				keptChecks := 0
				loop.SetDeliveryConversationKept(func(key string) bool { keptChecks++; return key == target })
				callbacks, legacyCallbacks := 0, 0
				var reported []memory.CommittedHistoryMessage
				loop.SetSessionHistoryReceiptObserver(
					func(_ string, items []memory.CommittedHistoryMessage) { callbacks++; reported = items },
				)
				loop.SetSessionHistoryObserver(func(string) { legacyCallbacks++ })
				const origin = "agent:cron-ownership-scan"
				loop.MirrorDelivery(
					tools.ExternalDelivery{
						Origin:  origin,
						Channel: "telegram",
						ChatID:  "123",
						Text:    "delivered despite unrelated storage failure",
					},
				)
				restore()
				history, readErr := store.GetHistory(t.Context(), target)
				if readErr != nil || len(history) != 1 ||
					history[0].Content != "delivered despite unrelated storage failure" ||
					history[0].Delivery == nil ||
					history[0].Delivery.Origin != origin ||
					history[0].Delivery.At.IsZero() {
					t.Fatal("a real kept-target MirrorDelivery was lost to another source's I/O", history, readErr)
				}
				if keptChecks == 0 || callbacks != 1 || legacyCallbacks != 1 {
					t.Fatal(
						"delivery did not traverse kept-target persistence/notification",
						keptChecks,
						callbacks,
						legacyCallbacks,
					)
				}
				if fault == "metadata permission" {
					if reported != nil {
						t.Fatal("uncertain other identity minted a receipt notification", reported)
					}
				} else if len(reported) != 1 || !reported[0].Order.Valid() || reported[0].Order.Sequence != 1 {
					t.Fatal("unrelated witness I/O prevented the healthy source receipt", reported)
				}
				if _, e := os.Stat(
					filepath.Join(dir, other+".jsonl.recovery-required"),
				); !errors.Is(
					e,
					os.ErrNotExist,
				) {
					t.Fatal("identity selection mutated the unrelated recovery fence", e)
				}
			})
		}
	}
}
