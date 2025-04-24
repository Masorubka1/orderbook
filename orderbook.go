package orderbook

import (
	"sync/atomic"

	"github.com/geseq/orderbook/pkg/pool"
	local_tree "github.com/geseq/orderbook/pkg/tree"
	decimal "github.com/geseq/udecimal"
)

// NotificationHandler handles notification updates
type NotificationHandler interface {
	PutOrder(m MsgType, s OrderStatus, orderID uint64, qty decimal.Decimal, err error)
	PutTrade(makerOrderID, takerOrderID uint64, makerStatus, takerStatus OrderStatus, qty, price decimal.Decimal)
}

var oPool = pool.NewItemPoolV2[Order](1)
var oqPool = pool.NewItemPoolV2[orderQueue](1)

// OrderBook implements standard matching algorithm
type OrderBook struct {
	bidsAsks     [2]*priceLevel
	triggerUnder *priceLevel                      // orders triggering under last price i.e. Stop Sell or Take Buy
	triggerOver  *priceLevel                      // orders that trigger over last price i.e. Stop Buy or Take Sell
	orders       *local_tree.Tree[uint64, *Order] // orderId -> *Order
	trigOrders   *local_tree.Tree[uint64, *Order] // orderId -> *Order
	trigQueue    *triggerQueue

	notification NotificationHandler

	lastPrice decimal.Decimal
	lastToken uint64
	cmpFuncs  [2]func(decimal.Decimal) bool

	matching bool

	orderPoolSize         uint64
	nodeTreePoolSize      uint64
	orderTreeNodePoolSize uint64
	orderQueuePoolSize    uint64
}

// Uint64Cmp compares two uint64.
//
//go:inline
func Uint64Cmp(a, b uint64) int {
	if a == b {
		return 0
	}
	if a < b {
		return -1
	}
	return 1
}

// NewOrderBook creates Orderbook object
func NewOrderBook(n NotificationHandler, opts ...Option) *OrderBook {
	ob := &OrderBook{
		orders:       local_tree.NewWithTree[uint64, *Order](Uint64Cmp, 1),
		trigOrders:   local_tree.NewWithTree[uint64, *Order](Uint64Cmp, 1),
		trigQueue:    newTriggerQueue(),
		bidsAsks:     [2]*priceLevel{newPriceLevel(BidPrice), newPriceLevel(AskPrice)},
		triggerUnder: newPriceLevel(TrigPrice),
		triggerOver:  newPriceLevel(TrigPrice),
		notification: n,
	}

	options(defaultOpts).applyTo(ob)
	options(opts).applyTo(ob)

	oPool = pool.NewItemPoolV2[Order](ob.orderPoolSize)
	oqPool = pool.NewItemPoolV2[orderQueue](ob.orderQueuePoolSize)
	ob.orders = local_tree.NewWithTree[uint64, *Order](Uint64Cmp, ob.orderTreeNodePoolSize)
	ob.trigOrders = local_tree.NewWithTree[uint64, *Order](Uint64Cmp, ob.orderTreeNodePoolSize)

	return ob
}

// AddOrder places new order to the OrderBook
// Arguments:
//
//	orderID   - unique order ID in depth (uint64)
//	class     - what class of order do you want to place (ob.Market or ob.Limit)
//	side      - what do you want to do (ob.Sell or ob.Buy)
//	quantity  - how much quantity you want to sell or buy (decimal)
//	price     - no more expensive (or cheaper) this price (decimal)
//	trigPrice - create a stop/take order until market price reaches this price (decimal)
//	flag      - immediate or cancel, all or none, fill or kill, Cancel (ob.IoC or ob.AoN or ob.FoK or ob.Cancel)
//	* to create new decimal number you should use udecimal.New() func
//	  read more at https://github.com/geseq/udecimal
func (ob *OrderBook) AddOrder(
	tok, id uint64,
	class ClassType,
	side SideType,
	quantity, price, trigPrice decimal.Decimal,
	flag FlagType,
) {
	// 1) Ensure determinism by checking the token sequence
	if !atomic.CompareAndSwapUint64(&ob.lastToken, tok-1, tok) {
		panic("invalid token received: cannot maintain determinism")
	}

	// 2) Basic validations
	if quantity.Equal(decimal.Zero) {
		ob.notification.PutOrder(MsgCreateOrder, Rejected, id, quantity, ErrInvalidQuantity)
		return
	}

	// Determine if this is a stop/take order
	isTrigger := flag&(StopLoss|TakeProfit) != 0
	if isTrigger {
		// Trigger orders must have a non-zero trigger price
		if trigPrice.IsZero() {
			ob.notification.PutOrder(MsgCreateOrder, Rejected, id, quantity, ErrInvalidTriggerPrice)
			return
		}
	} else if class != Market {
		// For limit orders: reject duplicates
		if _, exists := ob.orders.Get(id); exists {
			ob.notification.PutOrder(MsgCreateOrder, Rejected, id, decimal.Zero, ErrOrderExists)
			return
		}
		// For limit orders: price must be non-zero
		if price.Equal(decimal.Zero) {
			ob.notification.PutOrder(MsgCreateOrder, Rejected, id, decimal.Zero, ErrInvalidPrice)
			return
		}
	}

	// 3) If matching is disabled, reject any limit order that would cross the book
	if !ob.matching && class != Market {
		crossed := (side == Buy &&
			ob.bidsAsks[1].GetQueue() != nil &&
			ob.bidsAsks[1].GetQueue().Price().LessThanOrEqual(price)) ||
			(side == Sell &&
				ob.bidsAsks[0].GetQueue() != nil &&
				ob.bidsAsks[0].GetQueue().Price().GreaterThanOrEqual(price))
		if crossed {
			ob.notification.PutOrder(MsgCreateOrder, Rejected, id, quantity, ErrNoMatching)
			return
		}
	}

	// 4) Accept the order and notify
	ob.notification.PutOrder(MsgCreateOrder, Accepted, id, quantity, nil)
	o := NewOrder(id, class, side, quantity, price, trigPrice, flag)

	// 5) Route to trigger handling or immediate processing
	if isTrigger {
		ob.addTrigOrder(o)
	} else {
		ob.processOrder(o)
	}
}

func (ob *OrderBook) addTrigOrder(o *Order) {
	// Compute flag and side indices
	// flagIdx: 0 = StopLoss, 1 = TakeProfit (assuming o.Flag>>5 yields 0 or 1)
	// sideIdx: 1 - o.Side flips Buy=0→1, Sell=1→0
	flagIdx := int(o.Flag >> 5)
	sideIdx := int(1 - o.Side)

	// Flatten 2D arrays into 1D of length 4: index = flagIdx*2 + sideIdx
	idx := flagIdx*2 + sideIdx

	// priceLevels for deferred orders:
	pls := [4]*priceLevel{
		ob.triggerOver,  // StopLoss + Buy
		ob.triggerUnder, // StopLoss + Sell
		ob.triggerUnder, // TakeProfit + Buy
		ob.triggerOver,  // TakeProfit + Sell
	}

	// Which comparison to use:
	// true  = TrigPrice <= lastPrice
	// false = lastPrice <= TrigPrice
	useTrLE := [4]bool{
		true, false,
		false, true,
	}

	le := o.TrigPrice.LessThanOrEqual(ob.lastPrice)
	ge := ob.lastPrice.LessThanOrEqual(o.TrigPrice)

	if (useTrLE[idx] && le) || (!useTrLE[idx] && ge) {
		ob.processOrder(o)
	} else {
		ob.trigOrders.Put(o.ID, pls[idx].Append(o))
	}
}

func (ob *OrderBook) postProcess(lp decimal.Decimal) {
	if lp == ob.lastPrice {
		return
	}
	ob.queueTriggeredOrders()
	ob.processTriggeredOrders()
}

func (ob *OrderBook) processOrder(o *Order) {
	lp := ob.lastPrice

	pl := ob.bidsAsks[o.Side]
	var cmp func(decimal.Decimal) bool
	if o.Side == Buy {
		cmp = o.Price.GreaterThanOrEqual
	} else {
		cmp = o.Price.LessThanOrEqual
	}

	if o.Class == Market {
		pl.processMarketOrder(ob, o.ID, o.Qty, o.Flag)
	} else {
		executed := pl.processLimitOrder(ob, cmp, o.ID, o.Qty, o.Flag)

		// 3) If not IoC/FoK — put rest to the oposite side orderbook
		if o.Flag&(IoC|FoK) == 0 {
			if rem := o.Qty.Sub(executed); rem.GreaterThan(decimal.Zero) {
				newO := NewOrder(o.ID, o.Class, o.Side, rem, o.Price, decimal.Zero, o.Flag)
				opposite := ob.bidsAsks[1-o.Side]
				ob.orders.Put(newO.ID, opposite.Append(newO))
			}
		}
	}

	ob.postProcess(lp)
}

func (ob *OrderBook) queueTriggeredOrders() {
	if ob.lastPrice.IsZero() {
		return
	}

	lastPrice := ob.lastPrice

	for q := ob.triggerOver.MaxPriceQueue(); q != nil && lastPrice.LessThanOrEqual(q.price); q = ob.triggerOver.MaxPriceQueue() {
		for q.Len() > 0 {
			o := q.Head()
			ob.triggerOver.Remove(o)
			ob.trigQueue.Push(o)
		}
	}

	for q := ob.triggerUnder.MinPriceQueue(); q != nil && lastPrice.GreaterThanOrEqual(q.price); q = ob.triggerUnder.MinPriceQueue() {
		for q.Len() > 0 {
			o := q.Head()
			ob.triggerUnder.Remove(o)
			ob.trigQueue.Push(o)
		}
	}
}

func (ob *OrderBook) processTriggeredOrders() {
	for o := ob.trigQueue.Pop(); o != nil; o = ob.trigQueue.Pop() {
		ob.processOrder(o)
		o.Release()
	}
}

// Order returns order by id
func (ob *OrderBook) Order(orderID uint64) *Order {
	o, ok := ob.orders.Get(orderID)
	if !ok {
		o, ok := ob.trigOrders.Get(orderID)
		if !ok {
			return nil
		}

		return o
	}

	return o
}

// CancelOrder removes order with given ID from the order book
func (ob *OrderBook) CancelOrder(tok, orderID uint64) {
	if !atomic.CompareAndSwapUint64(&ob.lastToken, tok-1, tok) {
		panic("invalid token received: cannot maintain determinism")
	}

	o := ob.cancelOrder(orderID)
	if o == nil {
		ob.notification.PutOrder(MsgCancelOrder, Rejected, orderID, decimal.Zero, ErrOrderNotExists)
		return
	}

	ob.notification.PutOrder(MsgCancelOrder, Canceled, o.ID, o.Qty, nil)
	o.Release()
}

// CancelOrder removes order with given ID from the order book
func (ob *OrderBook) cancelOrder(orderID uint64) *Order {
	o, ok := ob.orders.Get(orderID)
	if !ok {
		return ob.cancelTrigOrders(orderID)
	}

	ob.orders.Remove(orderID)

	return ob.bidsAsks[1-o.Side].Remove(o)
}

func (ob *OrderBook) cancelTrigOrders(orderID uint64) *Order {
	triggerLevelMap := [2]*priceLevel{ob.triggerUnder, ob.triggerOver}
	o, ok := ob.trigOrders.Get(orderID)
	if !ok {
		return nil
	}

	ob.trigOrders.Remove(orderID)

	if (o.Flag & StopLoss) == 0 {
		return triggerLevelMap[1-o.Side].Remove(o)
	}

	return triggerLevelMap[o.Side].Remove(o)
}

// Ask returns the best (lowest priced) sell order (Ask) from the order book.
// It compares and swaps the `lastToken` to ensure that the provided token is correct
// and maintains deterministic behavior during the execution of the order matching.
//
// Parameters:
//   - tok: A unique token (uint64) used for deterministic order matching. The method
//     uses atomic operations to ensure that the token is updated correctly.
//
// Returns:
//   - *Order: The lowest-priced sell order (Ask) in the order book. If there are no
//     sell orders, the method returns nil.
//
// The method works as follows:
//  1. It uses `CompareAndSwapUint64` to check and update the last token, ensuring
//     that the token follows the expected order and is correctly managed.
//  2. It retrieves the `MinPriceQueue`, which represents the price level with the
//     lowest Ask (sell) orders.
//  3. It returns the first order in that queue using `Head()`.
//  4. If there are no orders, it returns `nil`.
func (ob *OrderBook) Ask(tok uint64) *Order {
	if !atomic.CompareAndSwapUint64(&ob.lastToken, tok-1, tok) {
		panic("invalid token received: cannot maintain determinism")
	}
	orderQueue := ob.bidsAsks[1].MinPriceQueue()
	if orderQueue == nil {
		return nil
	}

	return orderQueue.Head()
}

// Bid returns the best (highest priced) buy order (Bid) from the order book.
// Similar to `Ask()`, it uses atomic operations to ensure that the provided token
// is correct and ensures deterministic order processing.
//
// Parameters:
//   - tok: A unique token (uint64) used for deterministic order matching. The method
//     ensures the token is valid and uses atomic operations to update the token
//     correctly during execution.
//
// Returns:
//   - *Order: The highest-priced buy order (Bid) in the order book. If there are no
//     buy orders, the method returns nil.
//
// The method works as follows:
//  1. It uses `CompareAndSwapUint64` to ensure that the token is correct and
//     maintains order-matching determinism.
//  2. It retrieves the `MaxPriceQueue`, which represents the price level with the
//     highest Bid (buy) orders.
//  3. It returns the first order in that queue using `Head()`.
//  4. If there are no buy orders, it returns `nil`.
func (ob *OrderBook) Bid(tok uint64) *Order {
	if !atomic.CompareAndSwapUint64(&ob.lastToken, tok-1, tok) {
		panic("invalid token received: cannot maintain determinism")
	}
	orderQueue := ob.bidsAsks[0].MaxPriceQueue()
	if orderQueue == nil {
		return nil
	}

	return orderQueue.Head()
}
