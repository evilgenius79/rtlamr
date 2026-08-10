package r900

import (
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/bemasher/rtlamr/protocol"
	"github.com/bemasher/rtlamr/r900/gf"
)

// encodeParity solves for the 5 parity symbols that zero the syndromes of
// the shortened RS codeword used by R900: 16 data symbols, 10 zero symbols,
// 5 parity symbols.
func encodeParity(t *testing.T, f *gf.Field, data []byte) []byte {
	t.Helper()

	// Codeword with parity zeroed.
	var msg [31]byte
	copy(msg[:], data)

	// D_j: syndrome contribution of the data symbols alone.
	d := f.Syndrome(msg[:], 5, 29)

	// S_j = D_j ^ sum_t parity[t] * m_j^(4-t) where m_j = alpha^(29+j).
	// Solve M*parity = D over GF(32) by Gaussian elimination.
	var m [5][6]byte
	for j := 0; j < 5; j++ {
		for tt := 0; tt < 5; tt++ {
			m[j][tt] = f.Exp((29 + j) * (4 - tt))
		}
		m[j][5] = d[j]
	}

	for col := 0; col < 5; col++ {
		pivot := -1
		for row := col; row < 5; row++ {
			if m[row][col] != 0 {
				pivot = row
				break
			}
		}
		if pivot == -1 {
			t.Fatal("rs solve: singular matrix")
		}
		m[col], m[pivot] = m[pivot], m[col]

		inv := f.Inv(m[col][col])
		for k := col; k < 6; k++ {
			m[col][k] = f.Mul(m[col][k], inv)
		}
		for row := 0; row < 5; row++ {
			if row == col || m[row][col] == 0 {
				continue
			}
			factor := m[row][col]
			for k := col; k < 6; k++ {
				m[row][k] ^= f.Mul(factor, m[col][k])
			}
		}
	}

	parity := make([]byte, 5)
	for tt := 0; tt < 5; tt++ {
		parity[tt] = m[tt][5]
	}

	// Verify the solution actually zeroes the syndromes.
	copy(msg[26:], parity)
	for _, syn := range f.Syndrome(msg[:], 5, 29) {
		if syn != 0 {
			t.Fatal("rs solve: nonzero syndrome")
		}
	}

	return parity
}

// buildSymbols packs the R900 message fields into 16 5-bit data symbols and
// appends the 5 RS parity symbols.
func buildSymbols(t *testing.T, f *gf.Field, want R900) []byte {
	t.Helper()

	bits := fmt.Sprintf("%032b%08b%06b%02b%024b%02b%04b%02b",
		want.ID, want.Unkn1, want.NoUse, want.BackFlow,
		want.Consumption, want.Unkn3, want.Leak, want.LeakNow,
	)
	if len(bits) != 80 {
		t.Fatalf("field values out of range, got %d bits", len(bits))
	}

	symbols := make([]byte, 0, 21)
	for idx := 0; idx < 80; idx += 5 {
		sym, err := strconv.ParseUint(bits[idx:idx+5], 2, 8)
		if err != nil {
			t.Fatal(err)
		}
		symbols = append(symbols, byte(sym))
	}

	return append(symbols, encodeParity(t, f, symbols)...)
}

// writeWaveform renders quantization digits as OOK chip waveforms into signal.
func writeWaveform(signal []float32, offset, chipLength int, digits []byte) {
	for k, d := range digits {
		pattern := chipPatterns[d%3]
		high := float32(0)
		if d >= 3 {
			high = 1
		}
		for chip := 0; chip < 4; chip++ {
			v := high
			if pattern[chip] == 0 {
				v = 1 - high
			}
			for s := 0; s < chipLength; s++ {
				signal[offset+k*4*chipLength+chip*chipLength+s] = v
			}
		}
	}
}

func newTestParser(t *testing.T) (*Parser, *protocol.Decoder) {
	t.Helper()

	const chipLength = 8

	p := NewParser(chipLength).(*Parser)
	d := protocol.NewDecoder()
	d.RegisterProtocol(p)
	d.Allocate()

	return p, &d
}

func parseOnce(p *Parser, pkts []protocol.Data) []protocol.Message {
	msgCh := make(chan protocol.Message, 8)
	wg := new(sync.WaitGroup)
	wg.Add(1)
	p.Parse(pkts, msgCh, wg)
	wg.Wait()
	close(msgCh)

	var msgs []protocol.Message
	for msg := range msgCh {
		msgs = append(msgs, msg)
	}
	return msgs
}

func TestParseSyntheticMessage(t *testing.T) {
	p, d := newTestParser(t)

	want := R900{
		ID:          123456789,
		Unkn1:       0xA5,
		NoUse:       12,
		BackFlow:    1,
		Consumption: 1234567,
		Unkn3:       2,
		Leak:        9,
		LeakNow:     2,
	}

	symbols := buildSymbols(t, p.field, want)

	// Expand symbols to base-6 digit pairs.
	var digits []byte
	for _, sym := range symbols {
		digits = append(digits, sym/6, sym%6)
	}

	// First Parse call allocates the parser's internal buffers.
	parseOnce(p, nil)

	cfg := d.Cfg
	// Parse shifts p.signal down by BlockSize before filtering, so stage the
	// waveform one BlockSize above where it should land. A packet at index 0
	// has its payload at PreambleLength-SymbolLength after the shift.
	payloadIdx := cfg.PreambleLength - cfg.SymbolLength
	writeWaveform(p.signal, payloadIdx+cfg.BlockSize, cfg.ChipLength, digits)

	msgs := parseOnce(p, []protocol.Data{{Idx: 0}})

	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	got, ok := msgs[0].(R900)
	if !ok {
		t.Fatalf("expected R900 message, got %T", msgs[0])
	}

	// The synthetic waveform has carrier chips at exactly 1.0 power and
	// noise chips at exactly 0: RSSI must be 0dB and SNR must hit the cap.
	if got.RSSI != 0 {
		t.Fatalf("expected RSSI 0dB for full-scale synthetic signal, got %.2f", got.RSSI)
	}
	if got.SNR != 99 {
		t.Fatalf("expected capped SNR 99dB for noiseless synthetic signal, got %.2f", got.SNR)
	}

	got.checksum = [5]byte{}
	got.RSSI, got.SNR = 0, 0
	if got != want {
		t.Fatalf("decoded message mismatch:\ngot:  %s\nwant: %s", got, want)
	}

	if len(got.Headers()) != len(got.Record()) {
		t.Fatalf("Headers/Record length mismatch: %d != %d", len(got.Headers()), len(got.Record()))
	}
}

func TestParseRejectsCorruptedMessage(t *testing.T) {
	p, d := newTestParser(t)

	want := R900{ID: 987654321, Consumption: 424242, Leak: 3, LeakNow: 1}
	symbols := buildSymbols(t, p.field, want)

	// Corrupt one data symbol; the RS syndrome check must reject it.
	symbols[7] ^= 0x11

	var digits []byte
	for _, sym := range symbols {
		digits = append(digits, sym/6, sym%6)
	}

	parseOnce(p, nil)

	cfg := d.Cfg
	payloadIdx := cfg.PreambleLength - cfg.SymbolLength
	writeWaveform(p.signal, payloadIdx+cfg.BlockSize, cfg.ChipLength, digits)

	if msgs := parseOnce(p, []protocol.Data{{Idx: 0}}); len(msgs) != 0 {
		t.Fatalf("expected corrupted message to be rejected, got %d messages", len(msgs))
	}
}
