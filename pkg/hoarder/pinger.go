package hoarder

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/cortze/ipfs-cid-hoarder/pkg/db"
	"github.com/cortze/ipfs-cid-hoarder/pkg/models"
	"github.com/cortze/ipfs-cid-hoarder/pkg/p2p"

	"github.com/libp2p/go-libp2p-core/peer"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	log "github.com/sirupsen/logrus"
)

const (
	DialTimeout     = 20 * time.Second
	minIterTime     = 500 * time.Millisecond
	maxDialAttempts = 3 // Are three attempts enough?
	dialGraceTime   = 10 * time.Second
)

// CidPinger is the main object to schedule and monitor all the CID related metrics
type CidPinger struct {
	ctx context.Context
	sync.Mutex
	wg *sync.WaitGroup

	host         *p2p.Host
	dbCli        *db.DBClient
	pingInterval time.Duration
	rounds       int
	workers      int

	init  bool
	initC chan struct{}

	cidQ      *cidQueue
	pingTaskC chan *models.CidInfo
}

// NewCidPinger return a CidPinger with the given configuration
func NewCidPinger(
	ctx context.Context,
	wg *sync.WaitGroup,
	host *p2p.Host,
	dbCli *db.DBClient,
	pingInterval time.Duration,
	rounds int,
	workers int) *CidPinger {

	return &CidPinger{
		ctx:          ctx,
		wg:           wg,
		host:         host,
		dbCli:        dbCli,
		pingInterval: pingInterval,
		rounds:       rounds,
		cidQ:         newCidQueue(),
		initC:        make(chan struct{}),
		pingTaskC:    make(chan *models.CidInfo, workers), // TODO: hardcoded
		workers:      workers,
	}
}

// Run executes the main logic of the CID Pinger.
// 1. runs the queue logic that schedules the pings
// 2. launches the pinger pool that will perform all the CID monitoring calls
func (p *CidPinger) Run() {
	defer p.wg.Done()

	var pingOrchWG sync.WaitGroup
	var pingOrcDoneFlag bool
	var pingerWG sync.WaitGroup

	pingOrchWG.Add(1)
	// Generate the Ping Orchester
	go func(p *CidPinger, wg *sync.WaitGroup) {
		defer pingOrchWG.Done()

		logEntry := log.WithField("mod", "cid-orchester")
		// we need to wait until the first CID is added, wait otherwise
		<-p.initC

		// generate a timer to determine
		minTimeT := time.NewTicker(minIterTime)

		for {
			select {
			case <-p.ctx.Done():
				log.Info("shutdown was detected, closing Cid Ping Orchester")
				return

			default:
				// loop over the list of CIDs, and check whether they need to be pinged or not
				for _, c := range p.cidQ.cidArray {
					// check if ctx was was closed
					if p.ctx.Err() != nil {
						log.Info("shutdown was detected, closing Cid Ping Orchester")
						return
					}
					cStr := c.CID.Hash().B58String()
					// check if the time for next ping has arrived
					if time.Since(c.NextPing) < time.Duration(0) {
						logEntry.Debugf("not in time to ping %s", cStr)
						break
					}

					// increment Ping Counter
					c.IncreasePingCounter()

					// Add the CID to the pingTaskC
					p.pingTaskC <- c

					// check if they have reached the max-round counter
					// if yes, remove them from the cidQ.cidArray
					if c.GetPingCounter() >= p.rounds {
						// delete the CID from the list
						p.cidQ.removeCid(cStr)
						logEntry.Infof("finished pinging CID %s - max pings reached %d", cStr, p.rounds)
					}

				}

				// if CID pinger was initialized and there are no more CIDs to track, we are done with the study
				if p.cidQ.Len() == 0 {
					return
				}

				// reorg the array of CIDs from lowest next ping time to biggest one
				p.cidQ.sortCidList()

				// check if ticker for next iteration was raised
				<-minTimeT.C
			}
		}

	}(p, &pingOrchWG)

	// Launch CID pingers (workers)
	for pinger := 0; pinger < p.workers; pinger++ {
		pingerWG.Add(1)
		go func(p *CidPinger, wg *sync.WaitGroup, pingOrcDoneFlag *bool, pingerID int) {
			defer wg.Done()

			logEntry := log.WithField("pinger", pingerID)
			logEntry.Debug("Initialized")
			for {
				// check if the ping orcherster has finished
				if *pingOrcDoneFlag && len(p.pingTaskC) == 0 {
					logEntry.Info("no more pings to orchestrate, finishing worker")
					return
				}

				select {
				case c, ok := <-p.pingTaskC:
					// check if the models.CidInfo id not nil
					if !ok {
						logEntry.Warn("empty CID received from channel, finishing worker")
						return
					}

					cStr := c.CID.Hash().B58String()
					pingCounter := c.GetPingCounter()

					logEntry.Infof("pinging CID %s created by %d | round %d from host %d", cStr, c.Creator.String(), pingCounter, p.host.ID().String())

					// request the status of PR Holders
					cidFetchRes := models.NewCidFetchResults(c.CID, pingCounter)

					var wg sync.WaitGroup

					wg.Add(1)
					// DHT FindProviders call to see if the content is actually retrievable from the network
					go func(p *CidPinger, c *models.CidInfo, fetchRes *models.CidFetchResults) {
						defer wg.Done()
						var isRetrievable bool = false
						t := time.Now()
						providers, err := p.host.DHT.LookupForProviders(p.ctx, c.CID)
						pingTime := time.Since(t)
						fetchRes.FindProvDuration = pingTime
						if err != nil {
							logEntry.Warnf("unable to get the closest peers to cid %s - %s", cStr, err.Error())
						}
						// iter through the providers to see if it matches with the host's peerID
						for _, paddrs := range providers {
							if paddrs.ID == c.Creator {
								isRetrievable = true
							}
						}
						fmt.Println(time.Now(), "Providers for", c.CID.Hash().B58String(), "->", providers)

						cidFetchRes.IsRetrievable = isRetrievable
					}(p, c, cidFetchRes)

					wg.Add(1)
					// recalculate the closest k peers to the content
					go func(p *CidPinger, c *models.CidInfo, fetchRes *models.CidFetchResults) {
						defer wg.Done()
						t := time.Now()

						var hops dht.Hops

						closestPeers, err := p.host.DHT.GetClosestPeers(p.ctx, string(c.CID.Hash()), &hops)
						pingTime := time.Since(t)
						fetchRes.TotalHops = hops.Total
						fetchRes.HopsToClosest = hops.ToClosest
						fetchRes.GetClosePeersDuration = pingTime
						if err != nil {
							logEntry.Warnf("unable to get the closest peers to cid %s - %s", cStr, err.Error())
						}
						for _, peer := range closestPeers {
							cidFetchRes.AddClosestPeer(peer)
						}
					}(p, c, cidFetchRes)

					// Ping in parallel each of the PRHolders
					for _, peerInfo := range c.PRHolders {
						wg.Add(1)
						go func(wg *sync.WaitGroup, c *models.CidInfo, peerInfo *models.PeerInfo, fetchRes *models.CidFetchResults) {
							defer wg.Done()
							pingRes := p.PingPRHolder(c, pingCounter, peerInfo.GetAddrInfo())
							fetchRes.AddPRPingResults(pingRes)
						}(&wg, c, peerInfo, cidFetchRes)
					}

					wg.Wait()

					// update the finish time for the total fetch round
					cidFetchRes.FinishTime = time.Now()

					// add the fetch results to the array and persist it in the DB
					p.dbCli.AddFetchResult(cidFetchRes)

				case <-p.ctx.Done():
					logEntry.Info("shutdown detected, closing pinger")
					return
				}
			}

		}(p, &pingerWG, &pingOrcDoneFlag, pinger)
	}

	pingOrchWG.Wait()
	log.Infof("finished pinging the CIDs on %d rounds", p.rounds)

	pingOrcDoneFlag = true

	pingerWG.Wait()
	close(p.pingTaskC)

	log.Debug("done from the CID Pinger")
}

// AddCidInfo adds a new CID to the pinging queue
func (p *CidPinger) AddCidInfo(c *models.CidInfo) {
	if !p.init {
		p.init = true
		p.initC <- struct{}{}
	}
	p.cidQ.addCid(c)
}

// PingPRHolder dials a given PR Holder to check whether it's active or not, and whether it has the PRs or not
func (p *CidPinger) PingPRHolder(c *models.CidInfo, round int, pAddr peer.AddrInfo) *models.PRPingResults {
	logEntry := log.WithFields(log.Fields{
		"cid": c.CID.Hash().B58String(),
	})

	var active, hasRecords bool
	var connError string

	// connect the peer
	pingCtx, cancel := context.WithTimeout(p.ctx, DialTimeout)
	defer cancel()

	tstart := time.Now()

	// loop over max tries if the connection is connection refused/ connection reset by peer
	for att := 0; att < maxDialAttempts; att++ {
		// TODO: attempt at least to see if the connection refused
		err := p.host.Connect(pingCtx, pAddr)
		if err != nil {
			logEntry.Debugf("unable to connect peer %s for Cid %s - error %s", pAddr.ID.String(), c.CID.Hash().B58String(), err.Error())
			connError = p2p.ParseConError(err)
			// If the error is not linked to an connection refused or reset by peer, just break the look
			if connError != p2p.DialErrorConnectionRefused && connError != p2p.DialErrorStreamReset {
				break
			}
		} else {
			logEntry.Debugf("succesful connection to peer %s for Cid %s", pAddr.ID.String(), c.CID.Hash().B58String())
			active = true
			connError = p2p.NoConnError

			// if the connection was successful, request whether it has the records or not
			provs, _, err := p.host.DHT.GetProvidersFromPeer(p.ctx, pAddr.ID, c.CID.Hash())
			if err != nil {
				log.Warnf("unable to retrieve providers from peer %s - error: %s", pAddr.ID, err.Error())
			} else {
				logEntry.Debugf("providers for Cid %s from peer %s - %v\n", c.CID.Hash().B58String(), pAddr.ID.String(), provs)
			}
			// iter through the providers to see if it matches with the host's peerID
			for _, paddrs := range provs {
				if paddrs.ID == c.Creator {
					hasRecords = true
					fmt.Println(time.Now(), "Peer", pAddr.ID.String(), "reporting on", c.CID.Hash().B58String(), " -> ", paddrs)
				}
			}

			// close connection and exit loop
			err = p.host.Network().ClosePeer(pAddr.ID)
			if err != nil {
				logEntry.Errorf("unable to close connection to peer %s - %s", pAddr.ID.String(), err.Error())
			}
			break
		}
	}

	fetchTime := time.Since(tstart)

	return models.NewPRPingResults(
		c.CID,
		pAddr.ID,
		round,
		tstart,
		fetchTime,
		active,
		hasRecords,
		connError)
}

// cidQueue is a simple queue of CIDs that allows rapid access to content through maps,
// while being abel to sort the array by closer next ping time to determine which is
// the next soonest peer to ping
type cidQueue struct {
	sync.RWMutex

	cidMap   map[string]*models.CidInfo
	cidArray []*models.CidInfo
}

func newCidQueue() *cidQueue {
	return &cidQueue{
		cidMap:   make(map[string]*models.CidInfo),
		cidArray: make([]*models.CidInfo, 0),
	}
}

func (q *cidQueue) isCidAlready(c string) bool {
	q.RLock()
	defer q.RUnlock()

	_, ok := q.cidMap[c]
	return ok
}

func (q *cidQueue) addCid(c *models.CidInfo) {
	q.Lock()
	defer q.Unlock()

	q.cidMap[c.CID.Hash().B58String()] = c
	q.cidArray = append(q.cidArray, c)
}

func (q *cidQueue) removeCid(cStr string) {
	delete(q.cidMap, cStr)
	// check if len of the queue is only one
	if q.Len() == 1 {
		q.cidArray = make([]*models.CidInfo, 0)
		return
	}
	item := -1
	for idx, c := range q.cidArray {
		if c.CID.Hash().B58String() == cStr {
			item = idx
			break
		}
	}
	// check if the item was found
	if item >= 0 {
		q.cidArray = append(q.cidArray[:item], q.cidArray[(item+1):]...)
	}
}

func (q *cidQueue) getCid(cStr string) (*models.CidInfo, bool) {
	q.RLock()
	defer q.RUnlock()

	c, ok := q.cidMap[cStr]
	return c, ok
}

func (q *cidQueue) sortCidList() {
	sort.Sort(q)
}

// Swap is part of sort.Interface.
func (q *cidQueue) Swap(i, j int) {
	q.Lock()
	defer q.Unlock()

	q.cidArray[i], q.cidArray[j] = q.cidArray[j], q.cidArray[i]
}

// Less is part of sort.Interface. We use c.PeerList.NextConnection as the value to sort by.
func (q *cidQueue) Less(i, j int) bool {
	q.RLock()
	defer q.RUnlock()

	return q.cidArray[i].NextPing.Before(q.cidArray[j].NextPing)
}

// Len is part of sort.Interface. We use the peer list to get the length of the array.
func (q *cidQueue) Len() int {
	q.RLock()
	defer q.RUnlock()

	return len(q.cidArray)
}
