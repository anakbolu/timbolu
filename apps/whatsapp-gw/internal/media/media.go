// Package media keeps the bytes of what a customer sent.
//
// It stores them in the deployment's object storage under the same key the
// backend reads them back from — the workspace, the message, and the kind — so
// the process that downloads an attachment and the process that serves it to a
// screen agree on where it is without either asking the other.
package media

import (
	"context"
	"path"

	"github.com/google/uuid"

	"github.com/anakbolu/timbolu/packages/platform/storage"
	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// Store implements whatsappdomain.MediaStore over an object store.
type Store struct {
	store storage.Store
}

// New builds the media store.
func New(store storage.Store) *Store { return &Store{store: store} }

// Compile-time check that the store is the port the engine declares.
var _ whatsappdomain.MediaStore = (*Store)(nil)

// Put stores one attachment.
//
// The key names the workspace, so the bytes of one tenant are never reachable by
// guessing another's, and it names the message, so a stored object can be traced
// back to what it was.
func (m *Store) Put(ctx context.Context, scope whatsappdomain.Scope, messageID uuid.UUID, attachment whatsappdomain.Media, content []byte) (whatsappdomain.StoredObject, error) {
	name := filenameFor(attachment.Kind, messageID)
	key := path.Join("whatsapp", scope.WorkspaceID.String(), messageID.String(), name)

	object, err := m.store.Put(ctx, key, contentTypeFor(attachment.Kind), content)
	if err != nil {
		return whatsappdomain.StoredObject{}, err
	}

	return whatsappdomain.StoredObject{
		Key:         object.Key,
		ContentType: object.ContentType,
		Filename:    name,
		Size:        object.ByteSize,
	}, nil
}

// Get reads one stored attachment, which is what sending one needs.
func (m *Store) Get(ctx context.Context, key string) ([]byte, error) {
	return m.store.Get(ctx, key)
}

// filenameFor names a stored attachment.
func filenameFor(kind string, messageID uuid.UUID) string {
	extension := map[string]string{
		whatsappdomain.KindImage:   ".jpg",
		whatsappdomain.KindVideo:   ".mp4",
		whatsappdomain.KindAudio:   ".ogg",
		whatsappdomain.KindSticker: ".webp",
	}[kind]

	return kind + "-" + messageID.String() + extension
}

// contentTypeFor names what an attachment is.
func contentTypeFor(kind string) string {
	switch kind {
	case whatsappdomain.KindImage:
		return "image/jpeg"
	case whatsappdomain.KindVideo:
		return "video/mp4"
	case whatsappdomain.KindAudio:
		return "audio/ogg"
	case whatsappdomain.KindSticker:
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}
