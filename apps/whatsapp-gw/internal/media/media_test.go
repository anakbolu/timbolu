package media

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/packages/platform/storage"
	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The media store is where the bytes of what a customer sent end up. Two things
// matter and both are about agreement: the key names the workspace and the
// message, and the content type matches what was sent, so the process that
// downloads and the process that serves a screen read the same object the same
// way.

func newStore(t *testing.T) (*Store, storage.Store) {
	t.Helper()

	store, err := storage.NewLocal(t.TempDir())
	require.NoError(t, err)
	return New(store), store
}

func TestPutNamesTheWorkspaceAndTheMessage(t *testing.T) {
	store, backend := newStore(t)

	scope := whatsappdomain.Scope{WorkspaceID: uuid.New()}
	messageID := uuid.New()

	object, err := store.Put(context.Background(), scope, messageID,
		whatsappdomain.Media{Kind: whatsappdomain.KindImage}, []byte("jpeg-bytes"))
	require.NoError(t, err)

	require.Equal(t, "whatsapp/"+scope.WorkspaceID.String()+"/"+messageID.String()+"/image-"+messageID.String()+".jpg", object.Key)
	require.Equal(t, "image/jpeg", object.ContentType)
	require.Equal(t, int64(len("jpeg-bytes")), object.Size)

	stored, err := backend.Get(context.Background(), object.Key)
	require.NoError(t, err)
	require.Equal(t, []byte("jpeg-bytes"), stored)
}

func TestPutNamesWhatEachKindIs(t *testing.T) {
	store, _ := newStore(t)

	cases := map[string]string{
		whatsappdomain.KindImage:    "image/jpeg",
		whatsappdomain.KindVideo:    "video/mp4",
		whatsappdomain.KindAudio:    "audio/ogg",
		whatsappdomain.KindSticker:  "image/webp",
		whatsappdomain.KindDocument: "application/octet-stream",
	}

	for kind, contentType := range cases {
		t.Run(kind, func(t *testing.T) {
			object, err := store.Put(context.Background(), whatsappdomain.Scope{WorkspaceID: uuid.New()}, uuid.New(),
				whatsappdomain.Media{Kind: kind}, []byte("x"))
			require.NoError(t, err)
			require.Equal(t, contentType, object.ContentType)
		})
	}
}

func TestGetReadsBackWhatPutStored(t *testing.T) {
	store, _ := newStore(t)
	scope := whatsappdomain.Scope{WorkspaceID: uuid.New()}
	messageID := uuid.New()

	object, err := store.Put(context.Background(), scope, messageID, whatsappdomain.Media{Kind: whatsappdomain.KindAudio}, []byte("ogg"))
	require.NoError(t, err)

	content, err := store.Get(context.Background(), object.Key)
	require.NoError(t, err)
	require.Equal(t, []byte("ogg"), content)
}

func TestGetReportsAMissingObject(t *testing.T) {
	store, _ := newStore(t)

	_, err := store.Get(context.Background(), "whatsapp/nobody/nothing/image-x.jpg")
	require.Error(t, err)
}
