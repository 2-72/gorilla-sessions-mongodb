package mongodbstoregorilla

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoDBStore stores sessions using mongoDB as backend.
type MongoDBStore struct {
	coll    *mongo.Collection
	codecs  []securecookie.Codec
	options sessions.Options
}

// MongoDBStoreConfig is a configuration options for MongoDBStore
type MongoDBStoreConfig struct {

	// whether to create TTL index(https://docs.mongodb.com/manual/core/index-ttl/)
	// for the session document
	IndexTTL bool

	// gorilla-sessions options
	SessionOptions sessions.Options
}

type sessionDoc struct {
	ID       primitive.ObjectID `bson:"_id"`
	Data     string             `bson:"data"`
	Modified time.Time          `bson:"modified"`
}

var defaultConfig = MongoDBStoreConfig{
	IndexTTL: true,
	SessionOptions: sessions.Options{
		Path:     "/",
		MaxAge:   3600 * 24 * 30,
		HttpOnly: true,
	},
}

// NewMongoDBStoreWithConfig returns a new NewMongoDBStore with a custom MongoDBStoreConfig
func NewMongoDBStoreWithConfig(coll *mongo.Collection, cfg MongoDBStoreConfig, keyPairs ...[]byte) (*MongoDBStore, error) {
	codecs := securecookie.CodecsFromPairs(keyPairs...)
	for _, codec := range codecs {
		if sc, ok := codec.(*securecookie.SecureCookie); ok {
			sc.MaxAge(cfg.SessionOptions.MaxAge)
		}
	}
	store := &MongoDBStore{coll, codecs, cfg.SessionOptions}

	if !cfg.IndexTTL {
		return store, nil
	}

	return store, store.ensureIndexTTL()
}

// NewMongoDBStore returns a new NewMongoDBStore with default config
//
// defaultConfig := MongoDBStoreConfig{
// 	IndexTTL: true,
// 	SessionOptions: sessions.Options{
// 		Path:     "/",
// 		MaxAge:   3600 * 24 * 30,
// 		HttpOnly: true,
// 	},
// }
func NewMongoDBStore(col *mongo.Collection, keyPairs ...[]byte) (*MongoDBStore, error) {
	return NewMongoDBStoreWithConfig(col, defaultConfig, keyPairs...)
}

// Get returns a session for the given name after adding it to the registry.
//
// It returns a new session if the sessions doesn't exist. Access IsNew on
// the session to check if it is an existing session or a new one.
//
// It returns a new session and an error if the session exists but could
// not be decoded.
func (mstore *MongoDBStore) Get(r *http.Request, name string) (*sessions.Session, error) {
	return sessions.GetRegistry(r).Get(mstore, name)
}

// New returns a session for the given name without adding it to the registry.
//
// The difference between New() and Get() is that calling New() twice will
// decode the session data twice, while Get() registers and reuses the same
// decoded session after the first call.
func (mstore *MongoDBStore) New(r *http.Request, name string) (*sessions.Session, error) {
	session := sessions.NewSession(mstore, name)
	options := mstore.options
	session.Options = &options
	session.IsNew = true

	cookie, err := r.Cookie(name)
	if err != nil {
		return session, nil
	}
	err = securecookie.DecodeMulti(name, cookie.Value, &session.ID, mstore.codecs...)
	if err != nil {
		return session, err
	}

	found, err := mstore.load(session)
	if err != nil {
		return session, err
	}
	session.IsNew = !found

	return session, nil
}

// Save adds a single session to the response and persist session in mongoDB collection
//
// If the Options.MaxAge of the session is <= 0 then the session file will be
// deleted from the store path. With this process it enforces the properly
// session cookie handling so no need to trust in the cookie management in the
// web browser.
func (mstore *MongoDBStore) Save(r *http.Request, w http.ResponseWriter, session *sessions.Session) error {
	ctx := context.Background()

	var ID primitive.ObjectID
	if session.ID == "" {
		ID = primitive.NewObjectID()
		session.ID = ID.Hex()
	} else {
		newID, err := primitive.ObjectIDFromHex(session.ID)
		if err != nil {
			return err
		}
		ID = newID
	}

	if session.Options.MaxAge < 0 {
		_, err := mstore.coll.DeleteOne(ctx, bson.M{"_id": ID})
		if err != nil {
			return err
		}
		http.SetCookie(w, sessions.NewCookie(session.Name(), "", session.Options))

		return nil
	}

	encoded, err := securecookie.EncodeMulti(session.Name(), session.Values, mstore.codecs...)
	if err != nil {
		return err
	}
	sessDoc := &sessionDoc{
		ID:       ID,
		Modified: time.Now(),
		Data:     encoded,
	}
	if val, ok := session.Values["modified"]; ok {
		modified, ok := val.(time.Time)
		if !ok {
			return errors.New("mongodbstore: invalid modified value")
		}
		sessDoc.Modified = modified
	}
	_, err = mstore.coll.UpdateOne(ctx, bson.M{"_id": ID}, bson.M{"$set": sessDoc}, options.Update().SetUpsert(true))
	if err != nil {
		return err
	}
	encodedID, err := securecookie.EncodeMulti(session.Name(), session.ID, mstore.codecs...)
	if err != nil {
		return err
	}

	http.SetCookie(w, sessions.NewCookie(session.Name(), encodedID, session.Options))

	return nil
}

func (mstore *MongoDBStore) ensureIndexTTL() error {
	ctx := context.Background()

	indexName := "modified_at_TTL"

	cursor, err := mstore.coll.Indexes().List(ctx)
	if err != nil {
		return fmt.Errorf("mongodbstore: error ensuring TTL index. Unable to list indexes: %w", err)
	}

	for cursor.Next(ctx) {
		indexInfo := &struct {
			Name string `bson:"name"`
		}{}

		if err = cursor.Decode(indexInfo); err != nil {
			return fmt.Errorf("mongodbstore: error ensuring TTL index. Unable to decode bson index document %w", err)
		}

		if indexInfo.Name == indexName {
			return nil
		}

	}
	indexOpts := options.Index().
		SetExpireAfterSeconds(int32(mstore.options.MaxAge)).
		SetBackground(true).
		SetSparse(true).
		SetName(indexName)

	indexModel := mongo.IndexModel{
		Keys: bson.M{
			"modified_at": 1,
		},
		Options: indexOpts,
	}
	_, err = mstore.coll.Indexes().CreateOne(ctx, indexModel)
	if err != nil {
		return fmt.Errorf("mongodbstore: error ensuring TTL index. Unable to create index: %w", err)
	}

	return nil
}

func (mstore *MongoDBStore) load(sess *sessions.Session) (found bool, err error) {
	ID, err := primitive.ObjectIDFromHex(sess.ID)
	if err != nil {
		return false, err
	}
	ctx := context.Background()
	sessDoc := &sessionDoc{}
	err = mstore.coll.FindOne(ctx, bson.M{"_id": ID}).Decode(sessDoc)
	if sessDoc.ID.IsZero() {
		return false, nil
	}
	err = securecookie.DecodeMulti(sess.Name(), sessDoc.Data, &sess.Values, mstore.codecs...)
	if err != nil {
		return false, err
	}

	return true, err
}
