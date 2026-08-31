package uaptx

// MakerOrder is a signed, standing sell order for a UAP position, as
// produced by uap-js's signMakerOrder and published to an order relay
// (contrib/uap-indexer). It commits (via SIGHASH_SINGLE|ANYONECANPAY) only
// to the maker's own input and the payment output at the same index --
// the token's eventual destination is unconstrained, since
// CheckUapOutputConservation independently guarantees a valid covenant
// exists without caring who it is addressed to.
type MakerOrder struct {
	// ScriptSig is the maker's signature for spending Txid:Vout, already
	// computed against a transaction whose vout[0] is
	// {PaymentValue, PaymentScript}.
	ScriptSig []byte
	Txid      string
	Vout      uint32

	// PaymentScript/PaymentValue are the maker's asking price. PaymentScript
	// may be nil (matching JS's `paymentScript !== undefined` check) for an
	// order with no payment output at all -- see FillOrder.
	PaymentScript []byte
	PaymentValue  int64
}

// FillOrderOpts configures FillOrder.
type FillOrderOpts struct {
	// ToPubkey/Multiplier: the covenant the taker wants the token sent to.
	ToPubkey   []byte
	Multiplier int64

	// Origin is the lineage identity of the position being spent.
	// Used in the output covenant script to maintain lineage across transfers.
	Origin []byte

	// TokenValue is the position's full value (satoshis). The maker's
	// order does not carry this -- SIGHASH_SINGLE|ANYONECANPAY did not
	// commit to it -- so the taker must know it independently (e.g. from
	// uap-indexer) and gets rejected by consensus if wrong.
	TokenValue int64

	// TakerInputs are the taker's own payment UTXO(s): unsigned inputs
	// appended after the maker's. The taker must sign these separately
	// (e.g. via SignP2PKHInput) before broadcasting -- FillOrder only
	// assembles the transaction.
	TakerInputs []TxIn

	// ChangeScript/ChangeValue: the taker's change output, appended last.
	// ChangeValue == 0 means no change output (matching JS's `if
	// (opts.changeValue)` truthiness check).
	ChangeScript []byte
	ChangeValue  int64
}

// FillOrder completes a signed maker order (see MakerOrder): appends the
// taker's own payment input(s) and a covenant output sending the token to
// themselves, plus change, and returns the finished (but not yet
// taker-signed) transaction.
//
// The maker's input/payment output are kept at index 0 -- the same index
// the maker signed against -- since SIGHASH_SINGLE ties the signature to
// output N when the signed input ends up at index N in the final
// transaction; everything the taker adds comes after.
//
// The taker must still sign their own input(s) themselves before
// broadcasting; this only assembles the transaction, it doesn't complete
// it.
func FillOrder(order MakerOrder, opts FillOrderOpts) Tx {
	vin := make([]TxIn, 0, 1+len(opts.TakerInputs))
	vin = append(vin, TxIn{
		Txid:      order.Txid,
		Vout:      order.Vout,
		ScriptSig: order.ScriptSig,
		Sequence:  DefaultSequence,
	})
	for _, inp := range opts.TakerInputs {
		vin = append(vin, TxIn{
			Txid:      inp.Txid,
			Vout:      inp.Vout,
			ScriptSig: []byte{},
			Sequence:  DefaultSequence,
		})
	}

	vout := make([]TxOut, 0, 3)
	if order.PaymentScript != nil {
		vout = append(vout, TxOut{Value: order.PaymentValue, ScriptPubKey: order.PaymentScript})
	}
	vout = append(vout, TxOut{
		Value:        opts.TokenValue,
		ScriptPubKey: BuildTransferScript(opts.ToPubkey, opts.Multiplier, opts.Origin),
	})
	if opts.ChangeValue != 0 {
		vout = append(vout, TxOut{Value: opts.ChangeValue, ScriptPubKey: opts.ChangeScript})
	}

	return Tx{
		Version:  1,
		Locktime: 0,
		Vin:      vin,
		Vout:     vout,
	}
}
