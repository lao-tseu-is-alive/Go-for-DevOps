// Package mem contains an in-memory storage implementation of storage.Data.
// This is great for unit tests and demos. Our implementation uses a
// left-leaning red black tree for storage of entries by birthdays and maps
// for all other indexes. Filtering is done by searching all indexes for matches
// by each filter and if all matches succeed we stream the entry found.
package mem

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/biogo/store/llrb"

	pb "github.com/PacktPublishing/Go-for-DevOps/chapter/8/petstore/proto"
	"github.com/PacktPublishing/Go-for-DevOps/chapter/8/petstore/server/storage"
)

// birthdays represents a set of pets that share the same birthday with
// keys that are pet IDs. This is what we insert into our birthday tree.
type birthdays map[string]*pb.Pet

// Compare implements the llrb.Comparable.Compare().
func (bi birthdays) Compare(b llrb.Comparable) int {
	var ap, bp *pb.Pet
	// Get any entry in the map, all have the same birthday.
	for _, ap = range bi {
		break
	}
	for _, bp = range b.(birthdays) {
		break
	}

	// Ignore errors because we have to conform to a function def
	// and we should not be storing records with errors in the Birthday.
	at, _ := storage.BirthdayToTime(ap.Birthday)
	bt, _ := storage.BirthdayToTime(bp.Birthday)

	switch {
	case at.Before(bt):
		return -1
	case at.Equal(bt):
		return 0
	}
	return 1
}

// birthdayGet is what we use to search for a pets with a particular birthday.
type birthdayGet struct {
	*pb.Pet
}

// Compare implements the llrb.Comparable.Compare().
func (bi birthdayGet) Compare(b llrb.Comparable) int {
	// Ignore errors because we have to conform to a function def
	// and we should not be storing records with errors in the Birthday.
	at, _ := storage.BirthdayToTime(bi.Pet.Birthday)
	var bt time.Time
	switch v := b.(type) {
	case birthdayGet:
		bt, _ = storage.BirthdayToTime(v.Pet.Birthday)
	case birthdays:
		var p *pb.Pet
		for _, p = range v {
			break
		}
		bt, _ = storage.BirthdayToTime(p.Birthday)
	}

	switch {
	case at.Before(bt):
		return -1
	case at.Equal(bt):
		return 0
	}
	return 1
}

// Data implements storage.Data.
type Data struct {
	mu       sync.RWMutex // protects the items in this block
	birthday *llrb.Tree
	names    map[string]map[string]*pb.Pet
	ids      map[string]*pb.Pet
	types    map[pb.PetType]map[string]*pb.Pet

	// searches contains all the search calls that must be done
	// when we do a search. This is populated in New().
	searches []func(context.Context, *pb.SearchPetsReq) []string
}

// New is the constructor for Data.
func New() *Data {
	d := Data{
		names:    map[string]map[string]*pb.Pet{},
		ids:      map[string]*pb.Pet{},
		birthday: &llrb.Tree{},
		types:    map[pb.PetType]map[string]*pb.Pet{},
	}
	d.searches = []func(context.Context, *pb.SearchPetsReq) []string{
		d.byNames,
		d.byTypes,
		d.byBirthdays,
	}
	return &d
}

// AddPets implements storage.Data.AddPets().
func (d *Data) AddPets(ctx context.Context, pets []*pb.Pet) error {
	d.mu.RLock()
	// Make sure that none of these IDs somehow exist already.
	for _, p := range pets {
		if _, ok := d.ids[p.Id]; ok {
			return fmt.Errorf("pet with ID(%s) is already present", p.Id)
		}
	}
	d.mu.RUnlock()

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range pets {
		d.ids[p.Id] = p
		if v, ok := d.names[p.Name]; ok {
			v[p.Id] = p
		} else {
			d.names[p.Name] = map[string]*pb.Pet{
				p.Id: p,
			}
		}
		if v, ok := d.types[p.Type]; ok {
			v[p.Id] = p
		} else {
			d.types[p.Type] = map[string]*pb.Pet{
				p.Id: p,
			}
		}
		v := d.birthday.Get(birthdayGet{p})
		if v == nil {
			d.birthday.Insert(birthdays{p.Id: p})
			continue
		}
		v.(birthdays)[p.Id] = p
	}
	return nil
}

// DeletePets implements stroage.Data.DeletePets().
func (d *Data) DeletePets(ctx context.Context, ids []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, id := range ids {
		p, ok := d.ids[id]
		if !ok {
			continue
		}
		delete(d.ids, id)
		if v, ok := d.names[p.Name]; ok {
			if len(v) == 1 {
				delete(d.names, p.Name)
			} else {
				delete(v, id)
			}
		}
		if v, ok := d.types[p.Type]; ok {
			if len(v) == 1 {
				delete(d.types, p.Type)
			} else {
				delete(v, id)
			}
		}
		v := d.birthday.Get(birthdayGet{p})
		if v == nil {
			continue
		}
		if len(v.(birthdays)) == 1 {
			d.birthday.Delete(birthdayGet{p})
		}
		delete(v.(birthdays), p.Id)
	}
	return nil
}

// SearchPets implements storage.Data.SearchPets().
func (d *Data) SearchPets(ctx context.Context, filter *pb.SearchPetsReq) chan storage.SearchItem {
	petsCh := make(chan storage.SearchItem, 1)

	go func() {
		defer close(petsCh)
		d.searchPets(ctx, filter, petsCh)
	}()

	return petsCh
}

func (d *Data) searchPets(ctx context.Context, filter *pb.SearchPetsReq, out chan storage.SearchItem) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	filters := 0
	if len(filter.Names) > 0 {
		filters++
	}
	if len(filter.Types) > 0 {
		filters++
	}
	if filter.BirthdateRange != nil {
		filters++
	}

	// They didn't provide filters, so just return everything.
	if filters == 0 {
		d.returnAll(ctx, out)
		return
	}

	searchCh := make(chan []string, len(d.searches))
	wg := sync.WaitGroup{}
	wg.Add(len(d.searches))

	// Spin off our searches.
	for _, search := range d.searches {
		search := search
		go func() {
			defer wg.Done()
			r := search(ctx, filter)
			select {
			case <-ctx.Done():
			case searchCh <- r:
			}
		}()
	}
	// Wait for our searches to complete then close our searchCh.
	go func() { wg.Wait(); close(searchCh) }()

	// Collect all IDs from searches and count them. When one hits
	// the total number of filters send the matching pet to the caller.
	m := map[string]int{}
	matchCh := make(chan string, 1)
	go func() {
		defer close(matchCh)
		for ids := range searchCh {
			for _, id := range ids {
				count := m[id]
				count++
				m[id] = count
				if count == filters {
					matchCh <- id
				}
			}
		}
	}()

	// This handles all our matches getting returned.
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-matchCh:
			if !ok {
				return
			}
			out <- storage.SearchItem{Pet: d.ids[id]}
		}
	}
}

// returnAll streams all the pets that we have.
func (d *Data) returnAll(ctx context.Context, out chan storage.SearchItem) {
	for _, p := range d.ids {
		select {
		case <-ctx.Done():
			return
		case out <- storage.SearchItem{Pet: p}:
		}
	}
}

// byNames returns IDs of pets that have the names matched in the filter.
func (d *Data) byNames(ctx context.Context, filter *pb.SearchPetsReq) []string {
	var ids []string
	for _, n := range filter.Names {
		if ctx.Err() != nil {
			return nil
		}
		p, ok := d.names[n]
		if !ok {
			continue
		}
		for id := range p {
			ids = append(ids, id)
		}
	}
	return ids
}

// byTypes returns IDs of pets that have the types matched in the filter.
func (d *Data) byTypes(ctx context.Context, filter *pb.SearchPetsReq) []string {
	var ids []string
	for _, t := range filter.Types {
		if ctx.Err() != nil {
			return nil
		}
		p, ok := d.types[t]
		if !ok {
			continue
		}
		for id := range p {
			ids = append(ids, id)
		}
	}
	return ids
}

// byBirthdays returns IDs of pets that have the birthdays matched in the filter.
func (d *Data) byBirthdays(ctx context.Context, filter *pb.SearchPetsReq) []string {
	if filter.BirthdateRange == nil {
		return nil
	}

	var ids []string
	d.birthday.DoRange(
		func(c llrb.Comparable) (done bool) {
			for _, p := range c.(birthdays) {
				if ctx.Err() != nil {
					return true
				}
				ids = append(ids, p.Id)
			}
			return
		},
		birthdayGet{&pb.Pet{Birthday: filter.BirthdateRange.Start}},
		birthdayGet{&pb.Pet{Birthday: filter.BirthdateRange.End}},
	)
	if ctx.Err() != nil {
		return nil
	}
	return ids
}
