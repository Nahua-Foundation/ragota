package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// ORM data access. The point of these edges is not their count: it is that a
// write and a read of the same table land on the same key, so a parameter
// trace can cross from one to the other. Every case therefore asserts a key,
// and the cross-file cases are the ones that used to resolve to nothing.

func TestORMDataAccessShapes(t *testing.T) {
	tests := []struct {
		name   string
		lang   string
		file   string
		src    string
		writes []string
		reads  []string
		absent []string
	}{
		{
			name: "ef core dbset reached through a context field declared in another file",
			lang: "csharp", file: "OrderRepository.cs",
			src: `namespace eShop.Ordering.Infrastructure.Repositories;

public class OrderRepository : IOrderRepository
{
    private readonly OrderingContext _context;

    public Order Add(Order order)
    {
        return _context.Orders.Add(order).Entity;
    }

    public async Task<Order> GetAsync(int orderId)
    {
        return await _context.Orders.FindAsync(orderId);
    }
}
`,
			writes: []string{"db:orders"},
			reads:  []string{"db:orders"},
		},
		{
			name: "ef core dbset behind a two-hop receiver, chained across lines",
			lang: "csharp", file: "CatalogApi.cs",
			src: `namespace eShop.Catalog.API;

public static class CatalogApi
{
    public static async Task<Results> GetItems(CatalogServices services)
    {
        var totalItems = await services.Context.CatalogItems
            .LongCountAsync();
        services.Context.CatalogItems.Add(item);
        return null;
    }
}
`,
			writes: []string{"db:catalog_items"},
			reads:  []string{"db:catalog_items"},
		},
		{
			name: "ef core dbset declared in the same file keeps the entity's name",
			lang: "csharp", file: "CatalogContext.cs",
			src: `namespace eShop.Catalog.API.Infrastructure;

public class CatalogContext : DbContext
{
    public required DbSet<CatalogItem> CatalogItems { get; set; }

    public void Seed(CatalogItem item)
    {
        CatalogItems.Add(item);
    }
}
`,
			writes: []string{"db:catalog_items"},
		},
		{
			name: "ef core Set<T> for an entity with no dbset property",
			lang: "csharp", file: "IntegrationEventLogService.cs",
			src: `namespace eShop.IntegrationEventLogEF.Services;

public class IntegrationEventLogService
{
    private readonly TContext _context;

    public async Task<IEnumerable<Entry>> Retrieve(Guid transactionId)
    {
        return await _context.Set<IntegrationEventLogEntry>()
            .Where(e => e.TransactionId == transactionId)
            .ToListAsync();
    }

    public Task Save(IntegrationEventLogEntry eventLogEntry)
    {
        _context.Set<IntegrationEventLogEntry>().Add(eventLogEntry);
        return _context.SaveChangesAsync();
    }
}
`,
			writes: []string{"db:integration_event_log_entries"},
			reads:  []string{"db:integration_event_log_entries"},
		},
		{
			name: "an http context is not a database context",
			lang: "csharp", file: "Middleware.cs",
			src: `namespace App;

public class Middleware
{
    public async Task Invoke(HttpContext httpContext)
    {
        httpContext.Items.Add("k", "v");
        await httpContext.Response.WriteAsync("ok");
    }
}
`,
			absent: []string{"db:items", "db:responses"},
		},
		{
			name: "typeorm repository injected by its generic type",
			lang: "typescript", file: "order.service.ts",
			src: `export class OrderService {
  constructor(private readonly orderRepository: Repository<Order>) {}

  async create(dto: CreateOrderDto) {
    return this.orderRepository.save(dto);
  }

  async list() {
    return this.orderRepository.find({ where: { userId: 1 } });
  }
}
`,
			writes: []string{"db:orders"},
			reads:  []string{"db:orders"},
		},
		{
			name: "typeorm repository obtained from the data source, in the chain and in a binding",
			lang: "typescript", file: "repo.ts",
			src: `export async function byId(dataSource: DataSource, id: string) {
  return dataSource.getRepository(Order).findOneBy({ id });
}

export async function insert(dataSource: DataSource) {
  const repo = dataSource.getRepository(Order);
  await repo.save({});
}
`,
			writes: []string{"db:orders"},
			reads:  []string{"db:orders"},
		},
		{
			name: "mikroorm entity manager naming the entity at the call",
			lang: "typescript", file: "service.ts",
			src: `export class Service {
  async run(manager: EntityManager) {
    const teams = await manager.find(Team, { id: 1 });
    await manager.nativeDelete(toMikroORMEntity(IndexData), { id: 1 });
    return teams;
  }
}
`,
			writes: []string{"db:index_datas"},
			reads:  []string{"db:teams"},
		},
		{
			name: "an entity manager call whose first argument is not a class resolves nothing",
			lang: "typescript", file: "generic.ts",
			src: `export class Base {
  async run(manager: EntityManager) {
    return manager.find(this.entity, { id: 1 });
  }
}
`,
			absent: []string{"db:entities"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := parseFactsOrFail(t, tt.lang, tt.file, tt.src)
			for _, want := range tt.writes {
				if findEdge(facts.Edges, storage.EdgeWritesTo, want) == nil {
					t.Errorf("writes_to %q missing from %v", want, edgeNamesOfKind(facts.Edges, storage.EdgeWritesTo))
				}
			}
			for _, want := range tt.reads {
				if findEdge(facts.Edges, storage.EdgeReadsFrom, want) == nil {
					t.Errorf("reads_from %q missing from %v", want, edgeNamesOfKind(facts.Edges, storage.EdgeReadsFrom))
				}
			}
			for _, not := range tt.absent {
				for _, kind := range []string{storage.EdgeWritesTo, storage.EdgeReadsFrom} {
					if findEdge(facts.Edges, kind, not) != nil {
						t.Errorf("%s %q was emitted and should not be", kind, not)
					}
				}
			}
		})
	}
}

// The two EF Core paths must agree: DbSet<CatalogItem> pluralizes the singular
// entity, the property name CatalogItems is already plural, and both have to
// arrive at the same table or a write found one way never meets a read found
// the other.
func TestDbSetTableAgreesWithEntityName(t *testing.T) {
	tests := []struct{ entity, property, want string }{
		{entity: "CatalogItem", property: "CatalogItems", want: "catalog_items"},
		{entity: "Order", property: "Orders", want: "orders"},
		{entity: "Buyer", property: "Buyers", want: "buyers"},
		{entity: "CardType", property: "CardTypes", want: "card_types"},
	}
	for _, tt := range tests {
		fromEntity := tableName(tt.entity)
		fromProperty := dbSetTable(tt.property)
		if fromEntity != tt.want || fromProperty != tt.want {
			t.Errorf("%s/%s: entity path = %q, property path = %q, want %q",
				tt.entity, tt.property, fromEntity, fromProperty, tt.want)
		}
	}
}

// A project that names its tables declares one name and queries another: the
// ORM detector keys its edges on the name derived from the entity
// (db:catalog_items), while the table is called "catalog". The pairing that
// joins them is a db_table unit keyed on the declared name whose signature
// names the entity, which is what these cases assert — the derivation itself
// belongs to the linker (entityTableName in internal/graph).
func TestDeclaredTableNamesArePublishedAgainstTheirEntity(t *testing.T) {
	tests := []struct {
		name string
		lang string
		file string
		src  string
		// want maps the declared table's key to the entity its unit must name.
		want map[string]string
		// absent are keys no db_table unit may carry.
		absent []string
	}{
		{
			name: "ef core configuration class, entity from the interface and the Configure parameter",
			lang: "csharp", file: "CatalogItemEntityTypeConfiguration.cs",
			src: `namespace eShop.Catalog.API.Infrastructure.EntityConfigurations;

class CatalogItemEntityTypeConfiguration : IEntityTypeConfiguration<CatalogItem>
{
    public void Configure(EntityTypeBuilder<CatalogItem> builder)
    {
        builder.ToTable("Catalog");
        builder.Property(ci => ci.Name).HasMaxLength(50);
    }
}
`,
			want:   map[string]string{"db:catalog": "CatalogItem"},
			absent: []string{"db:catalog_items"},
		},
		{
			name: "ef core configuration class whose base list names no entity, only the parameter does",
			lang: "csharp", file: "OrderEntityTypeConfiguration.cs",
			src: `namespace eShop.Ordering.Infrastructure.EntityConfigurations;

class OrderEntityTypeConfiguration : IOrderingConfiguration
{
    public void Configure(EntityTypeBuilder<Order> orderConfiguration)
    {
        orderConfiguration.ToTable("orders");
    }
}
`,
			want: map[string]string{"db:orders": "Order"},
		},
		{
			name: "ef core model builder, chained and as a block, in OnModelCreating",
			lang: "csharp", file: "OrderingContext.cs",
			src: `namespace eShop.Ordering.Infrastructure;

public class OrderingContext : DbContext
{
    protected override void OnModelCreating(ModelBuilder modelBuilder)
    {
        modelBuilder.Entity<PaymentMethod>().ToTable("paymentmethods");
        modelBuilder.Entity<IntegrationEventLogEntry>(builder =>
        {
            builder.ToTable("IntegrationEventLog");
            builder.HasKey(e => e.EventId);
        });
    }
}
`,
			want: map[string]string{
				"db:paymentmethods":      "PaymentMethod",
				"db:integrationeventlog": "IntegrationEventLogEntry",
			},
		},
		{
			name: "ef core table qualified by a schema, positionally and by name",
			lang: "csharp", file: "AnalyticsConfiguration.cs",
			src: `namespace Analytics;

class EventConfiguration : IEntityTypeConfiguration<Event>
{
    public void Configure(EntityTypeBuilder<Event> builder)
    {
        builder.ToTable("events", "analytics");
    }
}

class AuditConfiguration : IEntityTypeConfiguration<AuditEntry>
{
    public void Configure(EntityTypeBuilder<AuditEntry> builder)
    {
        builder.ToTable("entries", schema: "audit");
    }
}
`,
			want: map[string]string{
				"db:analytics.events": "Event",
				"db:audit.entries":    "AuditEntry",
			},
		},
		{
			name: "a ToTable overload that names no table declares nothing",
			lang: "csharp", file: "TriggerConfiguration.cs",
			src: `namespace App;

class OrderConfiguration : IEntityTypeConfiguration<Order>
{
    public void Configure(EntityTypeBuilder<Order> builder)
    {
        builder.ToTable(t => t.HasTrigger("orders_audit"));
    }
}
`,
			absent: []string{"db:orders", "db:t"},
		},
		{
			name: "jpa entity renamed by @Table",
			lang: "java", file: "PetType.java",
			src: `package org.springframework.samples.petclinic.customers.model;

@Entity
@Table(name = "types")
public class PetType {
    private Integer id;
}
`,
			want:   map[string]string{"db:types": "PetType"},
			absent: []string{"db:pet_types"},
		},
		{
			name: "typeorm entity named positionally, and one that names nothing",
			lang: "typescript", file: "order.entity.ts",
			src: `@Entity('order_headers')
export class Order {
  id: string;
}

@Entity()
export class Shipment {
  id: string;
}
`,
			want: map[string]string{"db:order_headers": "Order", "db:shipments": "Shipment"},
		},
		{
			name: "entity named in an options object: typeorm spells it name, mikro-orm tableName",
			lang: "typescript", file: "entities.ts",
			src: `@Entity({ name: "order_headers" })
export class OrderEntity {
  id: string;
}

@Entity({ tableName: "order_line_item" })
export class OrderLineItemEntity {
  id: string;
}

@Entity({ abstract: true })
export class BaseEntity {
  id: string;
}
`,
			want: map[string]string{
				"db:order_headers":   "OrderEntity",
				"db:order_line_item": "OrderLineItemEntity",
				"db:base_entities":   "BaseEntity",
			},
			absent: []string{"db:order_entities", "db:order_line_item_entities"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := parseFactsOrFail(t, tt.lang, tt.file, tt.src)
			var got []string
			byKey := map[string]*storage.ASTUnit{}
			for _, u := range facts.Units {
				if u.Kind != storage.KindDBTable {
					continue
				}
				byKey[u.Qualified] = u
				got = append(got, u.Qualified+" "+u.Signature)
			}
			for key, entity := range tt.want {
				u := byKey[key]
				if u == nil {
					t.Errorf("no db_table unit keyed %q; units: %v", key, got)
					continue
				}
				if want := "entity:" + entity; u.Signature != want {
					t.Errorf("%s signature = %q, want %q", key, u.Signature, want)
				}
				if u.Name == "" {
					t.Errorf("%s has no unit name", key)
				}
			}
			for _, key := range tt.absent {
				if byKey[key] != nil {
					t.Errorf("db_table unit keyed %q was published and should not be; units: %v", key, got)
				}
			}
		})
	}
}

// The declared name only pays off if the linker's own derivation reaches the
// entity the unit names: the two halves of the join are computed by different
// packages from the same entity, so a divergence silently un-joins them.
// entityTableName in internal/graph must match tableName here.
func TestEntityDerivedKeysReachTheDeclaredTables(t *testing.T) {
	tests := []struct{ entity, derived string }{
		{entity: "CatalogItem", derived: "catalog_items"},
		{entity: "CatalogBrand", derived: "catalog_brands"},
		{entity: "CardType", derived: "card_types"},
		{entity: "ClientRequest", derived: "client_requests"},
		{entity: "IntegrationEventLogEntry", derived: "integration_event_log_entries"},
		{entity: "PetType", derived: "pet_types"},
		{entity: "OrderLineItemEntity", derived: "order_line_item_entities"},
	}
	for _, tt := range tests {
		if got := tableName(tt.entity); got != tt.derived {
			t.Errorf("tableName(%q) = %q, want %q", tt.entity, got, tt.derived)
		}
	}
}

func TestEFEntityArg(t *testing.T) {
	tests := []struct{ text, want string }{
		{text: ": IEntityTypeConfiguration<CatalogItem>", want: "CatalogItem"},
		{text: "(EntityTypeBuilder<Order> orderConfiguration)", want: "Order"},
		{text: "modelBuilder.Entity<PaymentMethod>()", want: "PaymentMethod"},
		{text: "builder.Entity<eShop.Ordering.Order>", want: "Order"},
		{text: ": IOrderingConfiguration", want: ""},
		{text: "(ModelBuilder modelBuilder)", want: ""},
		{text: "builder.ToTable", want: ""},
	}
	for _, tt := range tests {
		if got := efEntityArg(tt.text); got != tt.want {
			t.Errorf("efEntityArg(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestTSEntityArg(t *testing.T) {
	tests := []struct{ expr, want string }{
		{expr: "Order", want: "Order"},
		{expr: "entities.Order", want: "Order"},
		{expr: "toMikroORMEntity(IndexData)", want: "IndexData"},
		{expr: "this.entity", want: ""},
		{expr: "{ id: 1 }", want: ""},
		{expr: `"orders"`, want: ""},
		{expr: "buildQuery(a, b)", want: ""},
	}
	for _, tt := range tests {
		if got := tsEntityArg(tt.expr); got != tt.want {
			t.Errorf("tsEntityArg(%q) = %q, want %q", tt.expr, got, tt.want)
		}
	}
}
