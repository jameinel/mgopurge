// Copyright 2018 Canonical Ltd.
// Licensed under the LGPLv3, see LICENCE file for details.

package txn

import (
	"time"

	"github.com/juju/errors"
	"github.com/juju/lru"
	"github.com/kr/pretty"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

const pruneTxnBatchSize = 1000
const queryDocBatchSize = 100
const pruneDocCacheSize = 10000

// IncrementalPruner reads the transzaction table incrementally, seeing if it can remove the current set of transactions,
// and then moves on to newer transactions. It only thinks about 1k txns at a time, because that is the batch size that
// can be deleted. Instead, it caches documents that it has seen.
type IncrementalPruner struct {
	docCache     *lru.LRU
	missingCache *lru.LRU
	stats        PrunerStats
}

// PrunerStats collects statistics about how the prune progressed
type PrunerStats struct {
	DocCacheHits       int
	DocCacheMisses     int
	DocMissingCacheHit int
	DocsMissing        int
	CollectionQueries  int
	DocReads           int
	DocStillMissing    int
	StashQueries       int
	StashReads         int
	DocQueuesCleaned   int
	DocTokensCleaned   int
	DocsAlreadyClean   int
	TxnsRemoved        int
	TxnsNotRemoved     int
}

func (p *IncrementalPruner) lookupDocsInCache(keys docKeySet) (docMap, map[string][]interface{}) {
	docs := make(docMap, len(docKeySet{}))
	docsByCollection := make(map[string][]interface{}, 0)
	for key, _ := range keys {
		cacheDoc, exists := p.docCache.Get(key)
		if exists {
			// Found in cache.
			// Note that it is possible we'll actually be looking at a document that has since been updated.
			// However, it is ok for new transactions to be added to the queue, and for completed transactions
			// to be removed.
			// The key for us is that we're only processing very old completed transactions, so the old information
			// we are looking at won't be changing. At worst we'll try to cleanup a document that has already been
			// cleaned up. But since we only process completed transaction we can't miss a document that has the txn
			// added to it.
			docs[key] = cacheDoc.(docWithQueue)
			p.stats.DocCacheHits++
		} else {
			p.stats.DocCacheMisses++
			docsByCollection[key.Collection] = append(docsByCollection[key.Collection], key.DocId)
		}
	}
	return docs, docsByCollection
}

func (p *IncrementalPruner) updateDocsFromCollections(
	docs docMap,
	docsByCollection map[string][]interface{},
	db *mgo.Database,
) (map[stashDocKey]struct{}, error) {
	missingKeys := make(map[stashDocKey]struct{}, 0)
	for collection, ids := range docsByCollection {
		missing := make(map[interface{}]struct{}, len(ids))
		for _, id := range ids {
			missing[id] = struct{}{}
		}
		coll := db.C(collection)
		query := coll.Find(bson.M{"_id": bson.M{"$in": ids}})
		query.Select(bson.M{"_id": 1, "txn-queue": 1})
		query.Batch(queryDocBatchSize)
		iter := query.Iter()
		p.stats.CollectionQueries++
		var doc docWithQueue
		for iter.Next(&doc) {
			key := docKey{Collection: collection, DocId: doc.Id}
			p.docCache.Add(key, doc)
			docs[key] = doc
			p.stats.DocReads++
			delete(missing, doc.Id)
		}
		p.stats.DocStillMissing += len(missing)
		for id, _ := range missing {
			stashKey := stashDocKey{Collection: collection, Id: id}
			missingKeys[stashKey] = struct{}{}
		}

		if err := iter.Close(); err != nil {
			return nil, errors.Trace(err)
		}
	}
	return missingKeys, nil
}

func (p *IncrementalPruner) updateDocsFromStash(
	docs docMap,
	missingKeys map[stashDocKey]struct{},
	txnsStash *mgo.Collection,
) error {
	// Note: there is some danger that new transactions will be adding and removing a document that we
	// reference in an old transaction. If that is happening fast enough, it is possible that we won't be able to see
	// the document in either place, and thus won't be able to verify that the old transaction is not actually
	// referenced. However, the act of adding or remove a document should be cleaning up the txn queue anyway,
	// which means it is safe to delete the document
	// For all the other documents, now we need to check txns.stash
	foundMissingKeys := make(map[stashDocKey]struct{}, len(missingKeys))
	p.stats.StashQueries++
	missingSlice := make([]stashDocKey, 0, len(missingKeys))
	for key := range missingKeys {
		missingSlice = append(missingSlice, key)
	}
	query := txnsStash.Find(bson.M{"_id": bson.M{"$in": missingSlice}})
	query.Select(bson.M{"_id": 1, "txn-queue": 1})
	query.Batch(queryDocBatchSize)
	iter := query.Iter()
	var doc stashEntry
	for iter.Next(&doc) {
		key := docKey{Collection: doc.Id.Collection, DocId: doc.Id.Id}
		qDoc := docWithQueue{Id: doc.Id.Id, Queue: doc.Queue}
		p.docCache.Add(key, qDoc)
		docs[key] = qDoc
		p.stats.StashReads++
		foundMissingKeys[doc.Id] = struct{}{}
	}
	if err := iter.Close(); err != nil {
		return errors.Trace(err)
	}
	for stashKey := range missingKeys {
		if _, exists := foundMissingKeys[stashKey]; exists {
			continue
		}
		// Note: we don't track docKeys that are still missing, they are found by the caller when they aren't in docMap
	}
	return nil
}

// lookupDocs searches the cache and then looks in the database for the txn-queue of all the referenced document keys.
func (p *IncrementalPruner) lookupDocs(keys docKeySet, txnsStash *mgo.Collection) (docMap, error) {
	docs, docsByCollection := p.lookupDocsInCache(keys)
	missingKeys, err := p.updateDocsFromCollections(docs, docsByCollection, txnsStash.Database)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(missingKeys) > 0 {
		err := p.updateDocsFromStash(docs, missingKeys, txnsStash)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	return docs, nil
}

func (p *IncrementalPruner) findTxnsAndDocsToLookup(iter *mgo.Iter) (bool, []txnDoc, map[bson.ObjectId]struct{}, docKeySet) {
	done := false
	// First, read all the txns to find the document identities we might care about
	txns := make([]txnDoc, 0, pruneTxnBatchSize)
	// We expect a doc in each txn
	docsToCheck := make(docKeySet, pruneTxnBatchSize)
	txnsBeingCleaned := make(map[bson.ObjectId]struct{})
	for count := 0; count < pruneTxnBatchSize; count++ {
		var txn txnDoc
		if iter.Next(&txn) {
			txns = append(txns, txn)
			for _, key := range txn.Ops {
				if _, ok := p.missingCache.Get(key); ok {
					// known to be missing, don't bother
					p.stats.DocMissingCacheHit++
					continue
				}
				docsToCheck[key] = struct{}{}
			}
			txnsBeingCleaned[txn.Id] = struct{}{}
		} else {
			done = true
		}
	}
	return done, txns, txnsBeingCleaned, docsToCheck
}

func (p *IncrementalPruner) findTxnsToPull(doc docWithQueue, txnsBeingCleaned map[bson.ObjectId]struct{}) ([]string, []string, []bson.ObjectId) {
	// We expect that *most* of the time, we won't pull any txns, because old txns will already have been removed
	// Because of this, we actually do 2 passes over the data. The first time we are seeing if there is anything that
	// might be pulled, and the second actually builds the new lists
	// DocQueuesCleaned:253,438,
	// DocsAlreadyClean:29,103,909
	// So about 100x more likely to not have anything to do. No need to allocate the slices we won't use.
	hasChanges := false
	for _, txnId := range doc.Txns() {
		if _, isCleaned := txnsBeingCleaned[txnId]; isCleaned {
			hasChanges = true
			break
		}
	}
	if !hasChanges {
		// No changes to make
		return nil, nil, nil
	}
	tokensToPull := make([]string, 0)
	newQueue := make([]string, 0, len(doc.Queue))
	newTxns := make([]bson.ObjectId, 0, len(doc.Queue))
	txnIds := doc.Txns()
	for i := range doc.Queue {
		token := doc.Queue[i]
		txnId := txnIds[i]
		if _, isCleaned := txnsBeingCleaned[txnId]; isCleaned {
			tokensToPull = append(tokensToPull, token)
		} else {
			newQueue = append(newQueue, token)
			newTxns = append(newTxns, txnId)
		}
	}
	return tokensToPull, newQueue, newTxns
}

func (p *IncrementalPruner) cleanupDocs(
	docsToCheck docKeySet,
	foundDocs docMap,
	txns []txnDoc,
	txnsBeingCleaned map[bson.ObjectId]struct{},
	db *mgo.Database,
	txnsStash *mgo.Collection,
) ([]bson.ObjectId, error) {
	txnsToDelete := make([]bson.ObjectId, 0, pruneTxnBatchSize)
	// TODO(jam): 2018-11-30 Currently this operates in txn order, iterating all the txns, finding docs to cleanup.
	// We could, instead, iterate the txn order, then build up documents in each collection to clean, and then issue
	// a single cleanup per collection.
	// However, I'm not sure how that interacts with txns.stash, as we need to know which documents we failed to update.
	// At least this code pulls all txns in the current batch in each pass. Though if you have the same docs over and
	// over, you end up iterating the list to find there is nothing to pull multiple times.
	for _, txn := range txns {
		txnCanBeRemoved := true
		for _, docKey := range txn.Ops {
			doc, ok := foundDocs[docKey]
			if !ok {
				p.stats.DocsMissing++
				if docKey.Collection == "metrics" {
					// XXX: This is a special case. Metrics are *known* to violate the transaction guarantees
					// by removing documents directly from the collection, without using a transaction. Even
					// though they are *created* with transactions... bad metrics, bad dog
					logger.Tracef("ignoring missing metrics doc: %v", docKey)
					p.missingCache.Add(docKey, nil)
				} else if docKey.Collection == "cloudimagemetadata" {
					// There is an upgrade step in 2.3.4 that bulk deletes all cloudimagemetadata that have particular
					// attributes, ignoring transactions...
					logger.Tracef("ignoring missing cloudimagemetadat doc: %v", docKey)
					p.missingCache.Add(docKey, nil)
				} else {
					logger.Warningf("transaction %q referenced document %v but it could not be found",
						txn.Id.Hex(), docKey)
					// This is usually a sign of corruption, but for the purposes of pruning, we'll just treat it as a
					// transaction that cannot be cleaned up.
					txnCanBeRemoved = false
				}
				continue
			}
			tokensToPull, newQueue, newTxns := p.findTxnsToPull(doc, txnsBeingCleaned)
			if len(tokensToPull) > 0 {
				p.stats.DocTokensCleaned += len(tokensToPull)
				p.stats.DocQueuesCleaned++
				coll := db.C(docKey.Collection)
				pull := bson.M{"$pullAll": bson.M{"txn-queue": tokensToPull}}
				err := coll.UpdateId(docKey.DocId, pull)
				if err != nil {
					if err != mgo.ErrNotFound {
						return nil, errors.Trace(err)
					}
					// Look in txns.stash
					err := txnsStash.UpdateId(stashDocKey{
						Collection: docKey.Collection,
						Id:         docKey.DocId,
					}, pull)
					if err != nil {
						if err == mgo.ErrNotFound {
							logger.Warningf("trying to cleanup doc %v, could not be found in collection nor stash",
								docKey)
							txnCanBeRemoved = false
							// We don't treat this as a fatal error, just a txn that cannot be cleaned up.
						}
						return nil, errors.Trace(err)
					}
				}
				// Update the known Queue of the document, since we cleaned it.
				doc.Queue = newQueue
				doc.txns = newTxns
				p.docCache.Add(docKey, doc)
			} else {
				// already clean of transactions we are currently processing
				p.stats.DocsAlreadyClean++
			}
		}
		if txnCanBeRemoved {
			txnsToDelete = append(txnsToDelete, txn.Id)
		} else {
			p.stats.TxnsNotRemoved++
		}
	}
	return txnsToDelete, nil
}

func (p *IncrementalPruner) pruneNextBatch(iter *mgo.Iter, txnsColl, txnsStash *mgo.Collection) (bool, error) {
	done, txns, txnsBeingCleaned, docsToCheck := p.findTxnsAndDocsToLookup(iter)
	// Now that we have a bunch of documents we want to look at, load them from the collections
	foundDocs, err := p.lookupDocs(docsToCheck, txnsStash)
	if err != nil {
		return done, errors.Trace(err)
	}
	txnsToDelete, err := p.cleanupDocs(docsToCheck, foundDocs, txns, txnsBeingCleaned, txnsColl.Database, txnsStash)
	if err != nil {
		return done, errors.Trace(err)
	}
	if len(txnsToDelete) > 0 {
		// TODO(jam): 2018-11-29 Evaluate if txnsColl.Bulk().RemoveAll is any better than txnsColl.RemoveAll, we especially want
		// to be using Unordered()
		// The other option is lots of Bulk.Remove() calls.
		// Bulk().Remove seems to be slower than RemoveAll
		results, err := txnsColl.RemoveAll(bson.M{
			"_id": bson.M{"$in": txnsToDelete},
		})
		p.stats.TxnsRemoved += results.Removed
		if err != nil {
			return done, errors.Trace(err)
		}
	}
	return done, nil
}

func (p *IncrementalPruner) Prune(args CleanAndPruneArgs) (PrunerStats, error) {
	tStart := time.Now()
	txns := args.Txns
	db := txns.Database
	txnsStashName := args.Txns.Name + ".stash"
	txnsStash := db.C(txnsStashName)
	query := txns.Find(completedOldTransactionMatch(args.MaxTime))
	query.Select(bson.M{
		"_id": 1,
		"o.c": 1,
		"o.d": 1,
	})
	// Sorting by _id helps make sure that we are grouping the transactions close to each other.
	query.Sort("_id")
	timer := newSimpleTimer(15 * time.Second)
	query.Batch(pruneTxnBatchSize)
	iter := query.Iter()
	for {
		// TODO(jam): 2018-11-29 Create 2 goroutines, so that we can be calling txns.Remove() while the other routine is0
		// reading docs, and cleaning up txn-queues. Not sure if that makes load bad, or if we get a 2x speedup because
		// we can use one connection for reading docs and txn-queues, and a different connection for txns.Remove()
		done, err := p.pruneNextBatch(iter, txns, txnsStash)
		if err != nil {
			iterErr := iter.Close()
			if iterErr != nil {
				logger.Warningf("ignoring iteration close error: %v", iterErr)
			}
			return p.stats, errors.Trace(err)
		}
		if done {
			break
		}
		if timer.isAfter() {
			logger.Debugf("pruning has removed %d txns, handling %d docs (%d in cache)",
				p.stats.TxnsRemoved, p.stats.DocCacheHits+p.stats.DocCacheMisses, p.stats.DocCacheHits)
		}
	}
	if err := iter.Close(); err != nil {
		return p.stats, errors.Trace(err)
	}
	// TODO: Now we should iterate over txns.Stash and remove documents that aren't referenced by any transactions.
	// Maybe we can just remove anything that
	logger.Infof("pruning removed %d txns and cleaned %d docs in %s.",
		p.stats.TxnsRemoved, p.stats.DocQueuesCleaned, time.Since(tStart).Round(time.Millisecond))
	logger.Debugf("prune stats: %s", pretty.Sprint(p.stats))
	return p.stats, nil
}

// docWithQueue is used to serialize a Mongo document that has a txn-queue
type docWithQueue struct {
	Id    interface{}     `bson:"_id"`
	Queue []string        `bson:"txn-queue"`
	txns  []bson.ObjectId `bson:"-"`
}

// Txns returns the Transaction ObjectIds associated with each token.
// These are cached on the doc object, so that we don't have to convert repeatedly.
func (dwq *docWithQueue) Txns() []bson.ObjectId {
	if dwq.txns != nil {
		return dwq.txns
	}
	dwq.txns = make([]bson.ObjectId, len(dwq.Queue))
	for i := range dwq.Queue {
		dwq.txns[i] = txnTokenToId(dwq.Queue[i])
	}
	return dwq.txns
}

// these are only the fields of txnDoc that we care about
type txnDoc struct {
	Id  bson.ObjectId `bson:"_id"`
	Ops []docKey      `bson:"o"`
}

type docKey struct {
	Collection string      `bson:"c"`
	DocId      interface{} `bson:"d"`
}

type stashDocKey struct {
	Collection string      `bson:"c"`
	Id         interface{} `bson:"id"`
}

type stashEntry struct {
	Id    stashDocKey `bson:"_id"`
	Queue []string    `bson:"txn-queue"`
}

type docKeySet map[docKey]struct{}

type docMap map[docKey]docWithQueue