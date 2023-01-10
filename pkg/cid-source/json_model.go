package cid_source

import (
	"encoding/json"
	"io/ioutil"
	"ipfs-cid-hoarder/pkg/config"
	"os"
	"reflect"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// JsonFileCIDSource reads CIDs and their content from a json file.
//When you want to access a new CID from the file the GetNewCid() function must be called.
//The Json contents are stored inside the struct and can be accessed with the index
type JsonFileCIDSource struct {
	filename string
	records  ProviderRecords
	iter     func() EncapsulatedJSONProviderRecord
}

func OpenMultipleSimpleJSONFiles(filenames []string) (*JsonFileCIDSource, error) {
	var recordsIn ProviderRecords
	for i := 0; i < len(filenames); i++ {
		jsonFile, err := os.Open(filenames[i])
		if err != nil {
			return nil, err
		}
		var records ProviderRecords

		byteValue, err := ioutil.ReadAll(jsonFile)

		if err != nil {
			return nil, errors.Wrap(err, "while trying to read the json file")
		}

		err = json.Unmarshal(byteValue, &records)
		if err != nil {
			return nil, errors.Wrap(err, "while trying to unmarshal json file contents")
		}
		recordsIn.EncapsulatedJSONProviderRecords = append(recordsIn.EncapsulatedJSONProviderRecords, records.EncapsulatedJSONProviderRecords...)
	}

	newjsonsource := &JsonFileCIDSource{
		records: recordsIn,
	}
	newjsonsource.initializeIter()
	return newjsonsource, nil
}

func OpenMultipleEncodedJSONFiles(filenames []string) (*JsonFileCIDSource, error) {
	var recordsIn ProviderRecords
	for i := 0; i < len(filenames); i++ {
		jsonFile, err := os.Open(filenames[i])
		if err != nil {
			return nil, err
		}
		var records ProviderRecords
		decoder := json.NewDecoder(jsonFile)

		for decoder.More() {
			err := decoder.Decode(&records)
			if err != nil {
				return nil, errors.Wrap(err, " while decoding encoded json")
			}
			recordsIn.EncapsulatedJSONProviderRecords = append(recordsIn.EncapsulatedJSONProviderRecords, records.EncapsulatedJSONProviderRecords...)
		}
	}

	newjsonsource := &JsonFileCIDSource{
		records: recordsIn,
	}
	newjsonsource.initializeIter()
	return newjsonsource, nil
}

//Opens and reads the content of a json file that has used the encode method to store the data
func OpenEncodedJSONFile(filename string) (*JsonFileCIDSource, error) {
	jsonFile, err := os.Open(filename)
	defer func(jsonFile *os.File) {
		err := jsonFile.Close()
		if err != nil {
			log.Errorf("failed to close json file: %s", err)
		}
	}(jsonFile)
	if err != nil {
		return nil, err
	}

	var records ProviderRecords
	decoder := json.NewDecoder(jsonFile)

	var filteredRecords ProviderRecords
	for decoder.More() {

		err := decoder.Decode(&records)
		if err != nil {
			return nil, errors.Wrap(err, " while decoding encoded json")
		}
		filteredRecords.EncapsulatedJSONProviderRecords = append(filteredRecords.EncapsulatedJSONProviderRecords, records.EncapsulatedJSONProviderRecords...)

	}

	newIn := &JsonFileCIDSource{
		filename: filename,
		records:  filteredRecords,
	}
	newIn.initializeIter()
	return newIn, nil
}

func OpenSimpleJSONFile(filename string) (*JsonFileCIDSource, error) {
	jsonFile, err := os.Open(filename)
	defer func(jsonFile *os.File) {
		err := jsonFile.Close()
		if err != nil {
			log.Errorf("failed to close file: %s", err)
		}
	}(jsonFile)
	if err != nil {
		return nil, err
	}

	var records ProviderRecords

	byteValue, err := ioutil.ReadAll(jsonFile)

	if err != nil {
		return nil, errors.Wrap(err, "while trying to read the json file")
	}

	err = json.Unmarshal(byteValue, &records)
	if err != nil {
		return nil, errors.Wrap(err, "while trying to unmarshal json file contents")
	}

	newIn := &JsonFileCIDSource{
		filename: filename,
		records:  records,
	}
	newIn.initializeIter()
	return newIn, nil
}

func (fileCIDSource *JsonFileCIDSource) initializeIter() {
	fileCIDSource.iter = fileCIDSource.nextEncapsulatedJSONProviderRecord()
}

func (fileCIDSource *JsonFileCIDSource) nextEncapsulatedJSONProviderRecord() func() EncapsulatedJSONProviderRecord {
	i := -1
	return func() EncapsulatedJSONProviderRecord {
		i++
		if i >= len(fileCIDSource.records.EncapsulatedJSONProviderRecords) {
			return EncapsulatedJSONProviderRecord{}
		}
		return fileCIDSource.records.EncapsulatedJSONProviderRecords[i]
	}
}

//Returns the json records read from the file when creating the file_cid_source instance.
func (fileCIDSource *JsonFileCIDSource) GetNewCid() (TrackableCid, error) {
	for true {
		pr := fileCIDSource.iter()
		if reflect.DeepEqual(pr, EncapsulatedJSONProviderRecord{}) {
			break
		}

		log.Debug("Read a new PR:")

		log.Debugf("It's cid is: %s", pr.CID)
		newCid, err := cid.Parse(pr.CID)
		if err != nil {
			log.Errorf("could not convert string to cid %s", err)
			continue
		}

		log.Debugf("It's peer id is: %s", pr.ID)
		newPid, err := peer.Decode(pr.ID)
		if err != nil {
			log.Errorf("could not convert string to pid %s", err)
			continue
		}

		log.Debugf("It's creator is: %s", pr.Creator)
		newCreator, err := peer.Decode(pr.Creator)
		if err != nil {
			log.Errorf("could not convert string to creator pid %s", err)
			continue
		}

		log.Debugf("It's provide time is: %s", pr.ProvideTime)
		newProvideTime, err := time.ParseDuration(pr.ProvideTime)

		if err != nil {
			log.Errorf("Error while parsing provide time: %s", err)
			continue
		}

		log.Debugf("It's publication time is: %s", pr.PublicationTime)
		newPublicationTime, err := time.Parse(layoutPublicationTime, pr.PublicationTime)

		if err != nil {
			log.Errorf("Error while parsing publication time: %s", err)
			continue
		}

		log.Debugf("It's user agent is: %s", pr.UserAgent)

		multiaddresses := make([]ma.Multiaddr, 0)
		for i := 0; i < len(pr.Addresses); i++ {
			multiaddr, err := ma.NewMultiaddr(pr.Addresses[i])
			if err != nil {
				//log.Errorf("could not convert string to multiaddress %s", err)
				continue
			}
			multiaddresses = append(multiaddresses, multiaddr)
		}

		log.Infof("generated new CID %s", newCid.Hash().B58String())

		log.Infof("Read a new provider ID %s.The multiaddresses are %v. The creator is %s. The new CID is %s", string(newPid), multiaddresses, newCreator, newCid)
		ProviderAndCidInstance := NewTrackableCid(newPid, newCid, newCreator, multiaddresses, newPublicationTime, newProvideTime, pr.UserAgent)

		return ProviderAndCidInstance, nil
	}
	return TrackableCid{}, errors.New("end of provider records")
}

//TODO type returning a string is not a good idea
func (fileCIDSource *JsonFileCIDSource) Type() string {
	return config.JsonFileSource
}
