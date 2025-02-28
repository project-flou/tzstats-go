// Copyright (c) 2020-2022 Blockwatch Data Inc.
// Author: alex@blockwatch.cc

package tzstats

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"blockwatch.cc/tzgo/micheline"
	"blockwatch.cc/tzgo/tezos"
)

type Op struct {
	Id            uint64              `json:"id"`
	Hash          tezos.OpHash        `json:"hash"`
	Type          OpType              `json:"type"`
	Block         tezos.BlockHash     `json:"block"`
	Timestamp     time.Time           `json:"time"`
	Height        int64               `json:"height"`
	Cycle         int64               `json:"cycle"`
	Counter       int64               `json:"counter"`
	OpN           int                 `json:"op_n"`
	OpP           int                 `json:"op_p"`
	Status        tezos.OpStatus      `json:"status"`
	IsSuccess     bool                `json:"is_success"`
	IsContract    bool                `json:"is_contract"`
	IsBatch       bool                `json:"is_batch,omitempty"`
	IsEvent       bool                `json:"is_event"`
	IsInternal    bool                `json:"is_internal"`
	GasLimit      int64               `json:"gas_limit"`
	GasUsed       int64               `json:"gas_used"`
	StorageLimit  int64               `json:"storage_limit"`
	StoragePaid   int64               `json:"storage_paid"`
	Volume        float64             `json:"volume"`
	Fee           float64             `json:"fee"`
	Reward        float64             `json:"reward"`
	Deposit       float64             `json:"deposit"`
	Burned        float64             `json:"burned"`
	TDD           float64             `json:"days_destroyed"`
	SenderId      uint64              `json:"sender_id"`
	ReceiverId    uint64              `json:"receiver_id"`
	CreatorId     uint64              `json:"creator_id"`
	BakerId       uint64              `json:"baker_id"`
	Sender        tezos.Address       `json:"sender"`
	Receiver      tezos.Address       `json:"receiver"`
	Creator       tezos.Address       `json:"creator"`                // origination
	Baker         tezos.Address       `json:"baker"`                  // delegation, origination
	PrevBaker     tezos.Address       `json:"previous_baker,notable"` // delegation
	Source        tezos.Address       `json:"source,notable"`         // internal operations
	Offender      tezos.Address       `json:"offender,notable"`       // double_x
	Accuser       tezos.Address       `json:"accuser,notable"`        // double_x
	Data          json.RawMessage     `json:"data,omitempty"`
	Errors        json.RawMessage     `json:"errors,omitempty"`
	Parameters    *ContractParameters `json:"parameters,omitempty"`   // transaction
	Storage       *ContractValue      `json:"storage,omitempty"`      // transaction, origination
	BigmapDiff    []BigmapUpdate      `json:"big_map_diff,omitempty"` // transaction, origination
	Value         micheline.Prim      `json:"value,omitempty"`        // register_constant
	Power         int                 `json:"power,omitempty"`        // endorsement
	Limit         *float64            `json:"limit,omitempty"`        // set deposits limit
	Confirmations int64               `json:"confirmations,notable"`
	BatchVolume   float64             `json:"batch_volume,omitempty,notable"`
	Entrypoint    string              `json:"entrypoint,omitempty,notable"`
	NOps          int                 `json:"n_ops,omitempty,notable"`
	Batch         []*Op               `json:"batch,omitempty,notable"`
	Internal      []*Op               `json:"internal,omitempty,notable"`
	Metadata      map[string]Metadata `json:"metadata,omitempty,notable"`

	columns  []string                 // optional, for decoding bulk arrays
	param    micheline.Type           // optional, may be decoded from script
	store    micheline.Type           // optional, may be decoded from script
	eps      micheline.Entrypoints    // optional, may be decoded from script
	bigmaps  map[int64]micheline.Type // optional, may be decoded from script
	withPrim bool
	withMeta bool
	onError  int
}

func (o *Op) BlockId() BlockId {
	return BlockId{
		Height: o.Height,
		Hash:   o.Block.Clone(),
		Time:   o.Timestamp,
	}
}

func (o *Op) Content() []*Op {
	list := []*Op{o}
	if len(o.Batch) == 0 && len(o.Internal) == 0 {
		return list
	}
	if o.IsBatch {
		list = list[:0]
		for _, v := range o.Batch {
			list = append(list, v)
			if len(v.Internal) > 0 {
				list = append(list, v.Internal...)
			}
		}
	}
	if len(o.Internal) > 0 {
		list = append(list, o.Internal...)
	}
	return list
}

func (o *Op) Cursor() uint64 {
	op := o
	if l := len(op.Batch); l > 0 {
		op = op.Batch[l-1]
	}
	if l := len(op.Internal); l > 0 {
		op = op.Internal[l-1]
	}
	return op.Id
}

func (o *Op) WithColumns(cols ...string) *Op {
	o.columns = cols
	return o
}

func (o *Op) WithScript(s *ContractScript) *Op {
	if s != nil {
		o.param, o.store, o.eps, o.bigmaps = s.Types()
	} else {
		o.param, o.store, o.eps, o.bigmaps = micheline.Type{}, micheline.Type{}, nil, nil
	}
	return o
}

func (o *Op) WithTypes(param, store micheline.Type, eps micheline.Entrypoints, b map[int64]micheline.Type) *Op {
	o.param = param
	o.store = store
	o.eps = eps
	o.bigmaps = b
	return o
}

func (o *Op) WithPrim(b bool) *Op {
	o.withPrim = b
	return o
}

func (o *Op) WithMeta(b bool) *Op {
	o.withMeta = b
	return o
}

func (o *Op) OnError(action int) *Op {
	o.onError = action
	return o
}

type OpList struct {
	Rows     []*Op
	withPrim bool
	columns  []string
	ctx      context.Context
	client   *Client
}

func (l OpList) Len() int {
	return len(l.Rows)
}

func (l OpList) Cursor() uint64 {
	if len(l.Rows) == 0 {
		return 0
	}
	// on table API only row_id is set
	return l.Rows[len(l.Rows)-1].Id
}

func (l *OpList) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Compare(data, []byte("null")) == 0 {
		return nil
	}
	if data[0] != '[' {
		return fmt.Errorf("OpList: expected JSON array")
	}
	array := make([]json.RawMessage, 0)
	if err := json.Unmarshal(data, &array); err != nil {
		return err
	}
	for _, v := range array {
		op := &Op{
			withPrim: l.withPrim,
			columns:  l.columns,
		}
		// we may need contract scripts
		if is, ok := getTableColumn(v, l.columns, "is_contract"); ok && is == "1" {
			recv, ok := getTableColumn(v, l.columns, "receiver")
			if ok && recv != "" && recv != "null" {
				addr, err := tezos.ParseAddress(recv)
				if err != nil {
					return fmt.Errorf("decode: invalid receiver address %s: %v", recv, err)
				}
				// load contract type info (required for decoding storage/param data)
				script, err := l.client.loadCachedContractScript(l.ctx, addr)
				if err != nil {
					return err
				}
				op = op.WithScript(script)
			}
		}
		if err := op.UnmarshalJSON(v); err != nil {
			return err
		}
		op.columns = nil
		l.Rows = append(l.Rows, op)
	}
	return nil
}

func (o *Op) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Compare(data, []byte("null")) == 0 {
		return nil
	}
	if len(data) == 2 {
		return nil
	}
	if data[0] == '[' {
		return o.UnmarshalJSONBrief(data)
	}
	type Alias *Op
	return json.Unmarshal(data, Alias(o))
}

func (o *Op) UnmarshalJSONBrief(data []byte) error {
	op := Op{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	unpacked := make([]interface{}, 0)
	err := dec.Decode(&unpacked)
	if err != nil {
		return err
	}
	for i, v := range o.columns {
		f := unpacked[i]
		if f == nil {
			continue
		}
		switch v {
		case "id":
			op.Id, err = strconv.ParseUint(f.(json.Number).String(), 10, 64)
		case "hash":
			op.Hash, err = tezos.ParseOpHash(f.(string))
		case "type":
			op.Type = ParseOpType(f.(string))
		case "block":
			op.Block, err = tezos.ParseBlockHash(f.(string))
		case "time":
			var ts int64
			ts, err = strconv.ParseInt(f.(json.Number).String(), 10, 64)
			if err == nil {
				op.Timestamp = time.Unix(0, ts*1000000).UTC()
			}
		case "height":
			op.Height, err = strconv.ParseInt(f.(json.Number).String(), 10, 64)
		case "cycle":
			op.Cycle, err = strconv.ParseInt(f.(json.Number).String(), 10, 64)
		case "counter":
			op.Counter, err = strconv.ParseInt(f.(json.Number).String(), 10, 64)
		case "op_n":
			op.OpN, err = strconv.Atoi(f.(json.Number).String())
		case "op_p":
			op.OpP, err = strconv.Atoi(f.(json.Number).String())
		case "status":
			op.Status = tezos.ParseOpStatus(f.(string))
		case "is_success":
			op.IsSuccess, err = strconv.ParseBool(f.(json.Number).String())
		case "is_contract":
			op.IsContract, err = strconv.ParseBool(f.(json.Number).String())
		case "is_batch":
			op.IsBatch, err = strconv.ParseBool(f.(json.Number).String())
		case "is_event":
			op.IsEvent, err = strconv.ParseBool(f.(json.Number).String())
		case "is_internal":
			op.IsInternal, err = strconv.ParseBool(f.(json.Number).String())
		case "gas_limit":
			op.GasLimit, err = strconv.ParseInt(f.(json.Number).String(), 10, 64)
		case "gas_used":
			op.GasUsed, err = strconv.ParseInt(f.(json.Number).String(), 10, 64)
		case "storage_limit":
			op.StorageLimit, err = strconv.ParseInt(f.(json.Number).String(), 10, 64)
		case "storage_paid":
			op.StoragePaid, err = strconv.ParseInt(f.(json.Number).String(), 10, 64)
		case "volume":
			op.Volume, err = f.(json.Number).Float64()
		case "fee":
			op.Fee, err = f.(json.Number).Float64()
		case "reward":
			op.Reward, err = f.(json.Number).Float64()
		case "deposit":
			op.Deposit, err = f.(json.Number).Float64()
		case "burned":
			op.Burned, err = f.(json.Number).Float64()
		case "days_destroyed":
			op.TDD, err = f.(json.Number).Float64()
		case "sender_id":
			op.SenderId, err = strconv.ParseUint(f.(json.Number).String(), 10, 64)
		case "receiver_id":
			op.ReceiverId, err = strconv.ParseUint(f.(json.Number).String(), 10, 64)
		case "creator_id":
			op.CreatorId, err = strconv.ParseUint(f.(json.Number).String(), 10, 64)
		case "baker_id":
			op.BakerId, err = strconv.ParseUint(f.(json.Number).String(), 10, 64)
		case "sender":
			op.Sender, err = tezos.ParseAddress(f.(string))
		case "receiver":
			op.Receiver, err = tezos.ParseAddress(f.(string))
		case "creator":
			op.Creator, err = tezos.ParseAddress(f.(string))
		case "baker":
			op.Baker, err = tezos.ParseAddress(f.(string))
		case "data":
			op.Data, err = json.Marshal(f)
		case "errors":
			op.Errors, err = json.Marshal(f)
		case "entrypoint":
			if op.Parameters == nil {
				op.Parameters = &ContractParameters{}
			}
			op.Parameters.Entrypoint = f.(string)
			op.Entrypoint = f.(string)
		case "parameters":
			var buf []byte
			if buf, err = hex.DecodeString(f.(string)); err == nil && len(buf) > 0 {
				params := &micheline.Parameters{}
				err = params.UnmarshalBinary(buf)
				if err == nil {
					op.Parameters = &ContractParameters{
						Entrypoint: params.Entrypoint,
					}
					ep, prim, _ := params.MapEntrypoint(o.param)
					if o.withPrim {
						op.Parameters.ContractValue.Prim = &prim
					}
					val := micheline.NewValue(ep.Type(), prim)
					val.Render = o.onError
					op.Parameters.ContractValue.Value, err = val.Map()
					if err != nil {
						err = fmt.Errorf("decoding params %s: %w", f.(string), err)
					}
				}
			}
		case "storage":
			var buf []byte
			if buf, err = hex.DecodeString(f.(string)); err == nil && len(buf) > 0 {
				prim := micheline.Prim{}
				err = prim.UnmarshalBinary(buf)
				if err == nil {
					op.Storage = &ContractValue{}
					if o.withPrim {
						op.Storage.Prim = &prim
					}
					if o.store.IsValid() {
						val := micheline.NewValue(o.store, prim)
						val.Render = o.onError
						op.Storage.Value, err = val.Map()
						if err != nil {
							err = fmt.Errorf("decoding storage %s: %w", f.(string), err)
						}
					}
				}
			}
		case "big_map_diff":
			var buf []byte
			if buf, err = hex.DecodeString(f.(string)); err == nil && len(buf) > 0 {
				bmd := make(micheline.BigmapEvents, 0)
				err = bmd.UnmarshalBinary(buf)
				if err == nil {
					op.BigmapDiff = make([]BigmapUpdate, len(bmd))
					for i, v := range bmd {
						var ktyp, vtyp micheline.Type
						if typ, ok := o.bigmaps[v.Id]; ok {
							ktyp, vtyp = typ.Left(), typ.Right()
						} else {
							ktyp = v.Key.BuildType()
						}
						op.BigmapDiff[i] = BigmapUpdate{
							Action:   v.Action,
							BigmapId: v.Id,
						}
						switch v.Action {
						case micheline.DiffActionAlloc, micheline.DiffActionCopy:
							// alloc/copy only
							op.BigmapDiff[i].KeyType = micheline.Type{Prim: v.KeyType}.TypedefPtr("@key")
							op.BigmapDiff[i].ValueType = micheline.Type{Prim: v.ValueType}.TypedefPtr("@value")
							op.BigmapDiff[i].SourceId = v.SourceId
							op.BigmapDiff[i].DestId = v.DestId
							if op.withPrim {
								op.BigmapDiff[i].KeyTypePrim = &v.KeyType
								op.BigmapDiff[i].ValueTypePrim = &v.ValueType
							}
						default:
							// update/remove only
							op.BigmapDiff[i].BigmapValue = BigmapValue{}
							if !v.Key.IsEmptyBigmap() {
								keybuf, _ := v.GetKey(ktyp).MarshalJSON()
								mk := MultiKey{}
								_ = mk.UnmarshalJSON(keybuf)
								op.BigmapDiff[i].BigmapValue.Key = mk
								op.BigmapDiff[i].BigmapValue.Hash = v.KeyHash
							}
							if o.withMeta {
								op.BigmapDiff[i].BigmapValue.Meta = &BigmapMeta{
									Contract:     op.Receiver,
									BigmapId:     v.Id,
									UpdateTime:   op.Timestamp,
									UpdateHeight: op.Height,
								}
							}
							if o.withPrim {
								op.BigmapDiff[i].BigmapValue.KeyPrim = &v.Key
							}
							if v.Action == micheline.DiffActionUpdate {
								// update only
								if o.withPrim {
									op.BigmapDiff[i].BigmapValue.ValuePrim = &v.Value
								}
								// unpack value if type is known
								if vtyp.IsValid() {
									val := micheline.NewValue(vtyp, v.Value)
									val.Render = o.onError
									op.BigmapDiff[i].BigmapValue.Value, err = val.Map()
									if err != nil {
										err = fmt.Errorf("decoding bigmap %d/%s: %w", v.Id, v.KeyHash, err)
									}
								}
							}
						}
						if err != nil {
							break
						}
					}
				}
			}
		}
		if err != nil {
			return err
		}
	}
	*o = op
	return nil
}

type OpQuery struct {
	tableQuery
}

func (c *Client) NewOpQuery() OpQuery {
	tinfo, err := GetTypeInfo(&Op{}, "")
	if err != nil {
		panic(err)
	}
	q := tableQuery{
		client:  c,
		Params:  c.params.Copy(),
		Table:   "op",
		Format:  FormatJSON,
		Limit:   DefaultLimit,
		Order:   OrderAsc,
		Columns: tinfo.FilteredAliases("notable"),
		Filter:  make(FilterList, 0),
	}
	return OpQuery{q}
}

func (q OpQuery) Run(ctx context.Context) (*OpList, error) {
	result := &OpList{
		columns:  q.Columns,
		ctx:      ctx,
		client:   q.client,
		withPrim: q.Prim,
	}
	if err := q.client.QueryTable(ctx, &q.tableQuery, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) QueryOps(ctx context.Context, filter FilterList, cols []string) (*OpList, error) {
	q := c.NewOpQuery()
	if len(cols) > 0 {
		q.Columns = cols
	}
	if len(filter) > 0 {
		q.Filter = filter
	}
	return q.Run(ctx)
}

type OpParams struct {
	Params
}

func NewOpParams() OpParams {
	return OpParams{NewParams()}
}

func (p OpParams) WithLimit(v uint) OpParams {
	p.Query.Set("limit", strconv.Itoa(int(v)))
	return p
}

func (p OpParams) WithOffset(v uint) OpParams {
	p.Query.Set("offset", strconv.Itoa(int(v)))
	return p
}

func (p OpParams) WithCursor(v uint64) OpParams {
	p.Query.Set("cursor", strconv.FormatUint(v, 10))
	return p
}

func (p OpParams) WithOrder(v OrderType) OpParams {
	p.Query.Set("order", string(v))
	return p
}

func (p OpParams) WithType(mode FilterMode, typs ...string) OpParams {
	if mode != "" {
		p.Query.Set("type."+string(mode), strings.Join(typs, ","))
	} else {
		p.Query.Del("type")
	}
	return p
}

func (p OpParams) WithBlock(v string) OpParams {
	p.Query.Set("block", v)
	return p
}

func (p OpParams) WithSince(v string) OpParams {
	p.Query.Set("since", v)
	return p
}

func (p OpParams) WithUnpack() OpParams {
	p.Query.Set("unpack", "1")
	return p
}

func (p OpParams) WithPrim() OpParams {
	p.Query.Set("prim", "1")
	return p
}

func (p OpParams) WithMeta() OpParams {
	p.Query.Set("meta", "1")
	return p
}

func (p OpParams) WithRights() OpParams {
	p.Query.Set("rights", "1")
	return p
}

func (p OpParams) WithMerge() OpParams {
	p.Query.Set("merge", "1")
	return p
}

func (p OpParams) WithStorage() OpParams {
	p.Query.Set("storage", "1")
	return p
}

func (c *Client) GetOp(ctx context.Context, hash tezos.OpHash, params OpParams) ([]*Op, error) {
	o := make([]*Op, 0)
	u := params.AppendQuery(fmt.Sprintf("/explorer/op/%s", hash))
	if err := c.get(ctx, u, nil, &o); err != nil {
		return nil, err
	}
	return o, nil
}
