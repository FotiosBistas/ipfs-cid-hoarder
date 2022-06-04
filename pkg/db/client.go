package db

import (
	"context"
	"os"
	"sync"

	"database/sql"

	"github.com/cortze/ipfs-cid-hoarder/pkg/models"
	"github.com/ipfs/go-cid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const bufferSize = 10000
const maxPersisters = 1

type DBClient struct {
	ctx context.Context
	m   sync.RWMutex

	dbPath string
	sqlCli *sql.DB

	// Req Channels
	cidInfoC     chan *models.CidInfo
	peerInfoC    chan DBPeer // might be unnecesary because could be taken from CID info when writing
	fetchResultC chan *models.CidFetchResults
	pingResultsC chan []*models.PRPingResults

	doneC chan struct{}
}

func RemoveOldDBIfExists(oldDBPath string) {
	os.Remove(oldDBPath)
}

func NewDBClient(ctx context.Context, dbPath string) (*DBClient, error) {
	logEntry := log.WithFields(log.Fields{
		"Path": dbPath,
	},
	)
	logEntry.Debug("initialising the db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, errors.Wrap(err, "unable to initialise SQLite3 db at "+dbPath)
	}

	// Ping database to verify connection.
	if err = db.Ping(); err != nil {
		return nil, errors.Wrap(err, "pinging database")
	}

	dbCli := &DBClient{
		ctx:          ctx,
		dbPath:       dbPath,
		sqlCli:       db,
		cidInfoC:     make(chan *models.CidInfo, bufferSize),
		peerInfoC:    make(chan DBPeer, bufferSize),
		fetchResultC: make(chan *models.CidFetchResults, bufferSize),
		pingResultsC: make(chan []*models.PRPingResults, bufferSize),
		doneC:        make(chan struct{}),
	}

	err = dbCli.initTables()
	if err != nil {
		return nil, errors.Wrap(err, "unable to initilize the tables for the SQLite3 DB")
	}

	go dbCli.runPersisters()

	logEntry.Infof("SQLite3 db initialised")
	return dbCli, nil
}

type DBPeer struct {
	Cid  *cid.Cid
	Peer *models.PeerInfo
}

func (db *DBClient) runPersisters() {
	log.Info("Initializing DB persisters")

	var persisterWG sync.WaitGroup
	for persister := 1; persister <= maxPersisters; persister++ {
		persisterWG.Add(4)
		go func(wg *sync.WaitGroup, persisterID int) {
			defer wg.Done()
			logEntry := log.WithField("persister", persisterID)
			for {
				select {
				case cidInfo := <-db.cidInfoC:
					logEntry.Debugf("persisting CID %s into DB", cidInfo.CID.Hash().B58String())
					err := db.addCidInfo(cidInfo)
					if err != nil {
						logEntry.Error("error persisting CidInfo - " + err.Error())
					}
				case <-db.doneC:
					logEntry.Info("finish detected, closing persister")
					return

				case <-db.ctx.Done():
					logEntry.Info("shutdown detected, closing persister")
					return
				}
			}
		}(&persisterWG, persister)
		go func(wg *sync.WaitGroup, persisterID int) {
			defer wg.Done()
			logEntry := log.WithField("persister", persisterID)
			for {
				select {
				case p := <-db.peerInfoC:
					logEntry.Debugf("persisting peer %s into DB", p.Peer.ID.String())
					err := db.addPeerInfo(p.Cid, p.Peer)
					if err != nil {
						logEntry.Error("error persisting PeerInfo - " + err.Error())
					}
				case <-db.doneC:
					logEntry.Info("finish detected, closing persister")
					return

				case <-db.ctx.Done():
					logEntry.Info("shutdown detected, closing persister")
					return
				}
			}
		}(&persisterWG, persister)
		go func(wg *sync.WaitGroup, persisterID int) {
			defer wg.Done()
			logEntry := log.WithField("persister", persisterID)
			for {
				select {
				case fetchResults := <-db.fetchResultC:
					err := db.addFetchResults(fetchResults)
					if err != nil {
						logEntry.Error("error persisting FetchResults - " + err.Error())
					}
				case <-db.doneC:
					logEntry.Info("finish detected, closing persister")
					return

				case <-db.ctx.Done():
					logEntry.Info("shutdown detected, closing persister")
					return
				}
			}
		}(&persisterWG, persister)
		go func(wg *sync.WaitGroup, persisterID int) {
			defer wg.Done()
			logEntry := log.WithField("persister", persisterID)
			for {
				select {
				case pingResults := <-db.pingResultsC:
					err := db.addPingResultsSet(pingResults)
					if err != nil {
						logEntry.Error("error persisting PingResults - " + err.Error())
					}

				case <-db.doneC:
					logEntry.Info("finish detected, closing persister")
					return

				case <-db.ctx.Done():
					logEntry.Info("shutdown detected, closing persister")
					return
				}
			}
		}(&persisterWG, persister)

	}

	persisterWG.Wait()
	log.Info("Done persisting study")
}

func (db *DBClient) AddCidInfo(c *models.CidInfo) {
	db.cidInfoC <- c
}

func (db *DBClient) AddPeerInfo(p DBPeer) {
	db.peerInfoC <- p
}

func (db *DBClient) AddFetchResult(f *models.CidFetchResults) {
	db.fetchResultC <- f
}

func (db *DBClient) AddPingResults(p []*models.PRPingResults) {
	db.pingResultsC <- p
}

func (db *DBClient) Close() {

	log.Info("closing SQLite3 DB for the CID Hoarder")

	// wait untill all the channels have been emptied

	// close the channels and send message on doneC
	db.doneC <- struct{}{}
	close(db.cidInfoC)
	close(db.peerInfoC)
	close(db.fetchResultC)
	close(db.pingResultsC)

	db.sqlCli.Close()
}

func (db *DBClient) initTables() error {
	var err error

	// cid_info table
	err = db.CreateCidInfoTable()
	if err != nil {
		return err
	}

	// peer_info table
	err = db.CreatePeerInfoTable()
	if err != nil {
		return err
	}

	// fetch_results table
	err = db.CreateFetchResultsTable()
	if err != nil {
		return err
	}

	// ping_results table
	err = db.CreatePingResultsTable()
	if err != nil {
		return err
	}
	return err
}
