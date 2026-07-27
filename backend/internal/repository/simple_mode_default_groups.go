package repository

import (
	"context"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const simpleModeDefaultGroupDescription = "Auto-created default group"

func ensureSimpleModeDefaultGroups(ctx context.Context, client *dbent.Client) error {
	if client == nil {
		return fmt.Errorf("nil ent client")
	}

	if err := backfillSimpleModeGrokDefaultImageGeneration(ctx, client); err != nil {
		return err
	}

	shouldSeed, err := shouldSeedSimpleModeDefaultGroups(ctx, client)
	if err != nil {
		return err
	}
	if !shouldSeed {
		return nil
	}

	requiredByPlatform := map[string]int{
		service.PlatformAnthropic:   1,
		service.PlatformOpenAI:      1,
		service.PlatformGemini:      1,
		service.PlatformAntigravity: 2,
		service.PlatformGrok:        1,
	}

	for platform, minCount := range requiredByPlatform {
		count, err := client.Group.Query().
			Where(group.PlatformEQ(platform), group.DeletedAtIsNil()).
			Count(ctx)
		if err != nil {
			return fmt.Errorf("count groups for platform %s: %w", platform, err)
		}

		if platform == service.PlatformAntigravity {
			if count < minCount {
				for i := count; i < minCount; i++ {
					name := fmt.Sprintf("%s-default-%d", platform, i+1)
					if err := createGroupIfNotExists(ctx, client, name, platform); err != nil {
						return err
					}
				}
			}
			continue
		}

		// Non-antigravity platforms: ensure <platform>-default exists.
		name := platform + "-default"
		if err := createGroupIfNotExists(ctx, client, name, platform); err != nil {
			return err
		}
	}

	return nil
}

// shouldSeedSimpleModeDefaultGroups only permits default-group creation for a
// genuinely fresh database. Migration 008 creates one legacy seed group named
// "default", so that exact row is treated as an empty-install marker. Any other
// group history means an operator has already shaped routing and must be left
// untouched during upgrades.
func shouldSeedSimpleModeDefaultGroups(ctx context.Context, client *dbent.Client) (bool, error) {
	count, err := client.Group.Query().Count(mixins.SkipSoftDelete(ctx))
	if err != nil {
		return false, fmt.Errorf("count groups before seeding simple mode defaults: %w", err)
	}
	if count == 0 {
		return true, nil
	}
	if count != 1 {
		return false, nil
	}

	isInitialSeed, err := client.Group.Query().
		Where(
			group.NameEQ("default"),
			group.DescriptionEQ("Default group"),
			group.DeletedAtIsNil(),
		).
		Exist(ctx)
	if err != nil {
		return false, fmt.Errorf("check initial default group before seeding simple mode defaults: %w", err)
	}
	if !isInitialSeed {
		return false, nil
	}

	hasUsers, err := client.User.Query().Exist(mixins.SkipSoftDelete(ctx))
	if err != nil {
		return false, fmt.Errorf("check user history before seeding simple mode defaults: %w", err)
	}
	if hasUsers {
		return false, nil
	}

	hasAccounts, err := client.Account.Query().Exist(mixins.SkipSoftDelete(ctx))
	if err != nil {
		return false, fmt.Errorf("check account history before seeding simple mode defaults: %w", err)
	}
	return !hasAccounts, nil
}

func createGroupIfNotExists(ctx context.Context, client *dbent.Client, name, platform string) error {
	exists, err := client.Group.Query().
		Where(group.NameEQ(name), group.DeletedAtIsNil()).
		Exist(ctx)
	if err != nil {
		return fmt.Errorf("check group exists %s: %w", name, err)
	}
	if exists {
		return nil
	}

	_, err = client.Group.Create().
		SetName(name).
		SetDescription(simpleModeDefaultGroupDescription).
		SetPlatform(platform).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1.0).
		SetIsExclusive(false).
		SetAllowImageGeneration(platform == service.PlatformGrok).
		Save(ctx)
	if err != nil {
		if dbent.IsConstraintError(err) {
			// Concurrent server startups may race on creation; treat as success.
			return nil
		}
		return fmt.Errorf("create default group %s: %w", name, err)
	}
	return nil
}

func backfillSimpleModeGrokDefaultImageGeneration(ctx context.Context, client *dbent.Client) error {
	_, err := client.Group.Update().
		Where(
			group.NameEQ(service.PlatformGrok+"-default"),
			group.PlatformEQ(service.PlatformGrok),
			group.DescriptionEQ(simpleModeDefaultGroupDescription),
			group.StatusEQ(service.StatusActive),
			group.AllowImageGenerationEQ(false),
			group.DeletedAtIsNil(),
		).
		SetAllowImageGeneration(true).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("backfill auto-created grok default image generation: %w", err)
	}
	return nil
}
