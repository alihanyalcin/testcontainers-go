package couchbase

import (
	"context"
	"github.com/couchbase/gocb/v2"
	"testing"
	"time"
)

func TestCouchbase(t *testing.T) {
	ctx := context.Background()

	bucketName := "testBucket"
	container, err := StartContainer(ctx, WithImageName("couchbase:7.1.3"), WithBucket(NewBucket(bucketName)))
	if err != nil {
		t.Fatal(err)
	}

	// Clean up the container after the test is complete
	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate container: %s", err)
		}
	})

	cluster, err := connectCluster(ctx, container)
	if err != nil {
		t.Fatalf("could not connect couchbase: %s", err)
	}

	bucket := cluster.Bucket(bucketName)

	err = bucket.WaitUntilReady(5*time.Second, nil)
	if err != nil {
		t.Fatalf("could not connect bucket: %s", err)
	}

	key := "foo"
	data := map[string]string{"key": "value"}
	collection := bucket.DefaultCollection()

	_, err = collection.Upsert(key, data, nil)
	if err != nil {
		t.Fatalf("could not upsert data: %s", err)
	}

	result, err := collection.Get(key, nil)
	if err != nil {
		t.Fatalf("could not get data: %s", err)
	}

	var resultData map[string]string
	err = result.Content(&resultData)
	if resultData["key"] != "value" {
		t.Errorf("Expected value to be [%s], got %s", "value", resultData["key"])
	}
}

func connectCluster(ctx context.Context, container *CouchbaseContainer) (*gocb.Cluster, error) {
	connectionString, err := container.ConnectionString(ctx)
	if err != nil {
		return nil, err
	}

	return gocb.Connect(connectionString, gocb.ClusterOptions{
		Username: container.Username(),
		Password: container.Password(),
	})
}
