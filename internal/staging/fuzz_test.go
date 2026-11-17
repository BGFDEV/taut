package staging

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// fuzzSeed is a genuine ciphertext and its plaintext.
type fuzzSeed struct{ stored, plain []byte }

const fuzzName = "runs/fuzz/object"

// fuzzSeeds are the only valid ciphertexts under the fuzz key and name: the
// fuzzer cannot forge a GCM tag, so anything else it finds that decrypts is
// a bug.
func fuzzSeeds(t testing.TB) []fuzzSeed {
	var seeds []fuzzSeed
	for i, n := range []int{0, 1, tcs - 1, tcs, tcs + 1, 2 * tcs, 3*tcs + 17} {
		plain := prngBytes(t, uint64(100+i), n)
		// Fixed salts: the fuzzing engine's worker processes must rebuild
		// exactly the same ciphertexts.
		mem := newMem()
		e := smallChunks(mem, testKey(3))
		e.rand = prng(uint64(200+i), saltSize)
		mustPut(t, e, fuzzName, plain)
		seeds = append(seeds, fuzzSeed{mem.raw(fuzzName), plain})
	}
	return seeds
}

// checkNoForgery decrypts data and fails unless the outcome is one of:
// success on input identical to a genuine ciphertext; or an error after
// releasing nothing but whole chunks of a genuine ciphertext that the input
// reproduces byte for byte.
func checkNoForgery(t testing.TB, seeds []fuzzSeed, data []byte) {
	got, err := decrypt(testKey(3), fuzzName, data) // must not panic
	if err == nil {
		for _, s := range seeds {
			if bytes.Equal(data, s.stored) && bytes.Equal(got, s.plain) {
				return
			}
		}
		t.Fatalf("an input that is not a genuine ciphertext decrypted to %d bytes: %x", len(got), data)
	}
	wantCode(t, err, "staging.corrupt")
	if len(got) == 0 {
		return
	}
	if len(got)%tcs == 0 {
		verified := HeaderSize + len(got)/tcs*tchunk
		for _, s := range seeds {
			if len(data) >= verified && len(s.stored) >= verified &&
				bytes.Equal(data[:verified], s.stored[:verified]) && bytes.HasPrefix(s.plain, got) {
				return
			}
		}
	}
	t.Fatalf("released %d bytes of plaintext that no genuine ciphertext accounts for: input %x", len(got), data)
}

func FuzzDecryptReader(f *testing.F) {
	seeds := fuzzSeeds(f)
	for _, s := range seeds {
		f.Add(s.stored)
		f.Add(s.stored[:len(s.stored)/2])
		f.Add(append(bytes.Clone(s.stored), s.stored[HeaderSize:]...))
	}
	f.Add([]byte{})
	f.Add([]byte(encMagic))
	f.Fuzz(func(t *testing.T, data []byte) {
		checkNoForgery(t, seeds, data)
	})
}

// TestDecryptMutations is a fast, deterministic cousin of the fuzz target
// that runs in the ordinary suite: structured mutations of genuine
// ciphertexts, and pure noise.
func TestDecryptMutations(t *testing.T) {
	seeds := fuzzSeeds(t)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20000; i++ {
		data := bytes.Clone(seeds[rng.IntN(len(seeds))].stored)
		for m := rng.IntN(3) + 1; m > 0; m-- {
			other := seeds[rng.IntN(len(seeds))].stored
			switch rng.IntN(7) {
			case 0: // flip a bit
				if len(data) > 0 {
					data[rng.IntN(len(data))] ^= 1 << rng.IntN(8)
				}
			case 1: // truncate
				data = data[:rng.IntN(len(data)+1)]
			case 2: // append noise
				for n := rng.IntN(2*tchunk) + 1; n > 0; n-- {
					data = append(data, byte(rng.Uint32()))
				}
			case 3: // delete a chunk-sized range
				if a := rng.IntN(len(data) + 1); a+tchunk <= len(data) {
					data = append(data[:a], data[a+tchunk:]...)
				}
			case 4: // append a piece of another genuine ciphertext
				data = append(data, other[rng.IntN(len(other)):]...)
			case 5: // splice: our head, another's tail
				cut := rng.IntN(min(len(data), len(other)) + 1)
				data = append(data[:cut], other[cut:]...)
			case 6: // overwrite the chunk size
				if len(data) >= HeaderSize {
					data[HeaderSize-1-rng.IntN(4)] = byte(rng.Uint32())
				}
			}
		}
		checkNoForgery(t, seeds, data)
	}
	for i := 0; i < 2000; i++ {
		noise := make([]byte, rng.IntN(4*tchunk))
		for j := range noise {
			noise[j] = byte(rng.Uint32())
		}
		if i%2 == 0 && len(noise) >= len(encMagic) {
			copy(noise, encMagic)
		}
		checkNoForgery(t, seeds, noise)
	}
}
