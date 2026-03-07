# Built-in Memory Provider

The built-in memory provider is the standard memory backend shipped with Memoh. It works with Memoh's memory pipeline and supports:

- Automatic memory extraction from conversations
- Semantic memory retrieval during chat
- Manual memory creation and editing
- Memory compaction and rebuild workflows

To configure it well, you usually assign:

- **Memory Model**: The LLM used for memory extraction and decision making
- **Embedding Model**: The embedding model used for dense vector search

---

## Creating a Built-in Provider

Manage providers from the **Memory Providers** page in the sidebar.

1. Navigate to the **Memory Providers** page.
2. Click **Add Memory Provider**.
3. Fill in the following fields:
   - **Name**: A display name for this provider.
   - **Provider Type**: Select `builtin`.
4. Click **Create**.

---

## Configuring a Built-in Provider

After creating a provider, select it from the sidebar and configure its settings.

| Field | Description |
|-------|-------------|
| **Name** | The display name shown in the UI. |
| **Provider Type** | The provider implementation. Currently this is `builtin` only. |
| **Memory Model** | Optional chat model used for memory extraction and memory-related decisions. |
| **Embedding Model** | Optional embedding model used for semantic vector search. |

### Managing Providers

- **Edit**: Select a provider and update its name or model bindings.
- **Delete**: Remove a provider you no longer use.

---

## Assigning a Memory Provider to a Bot

1. Navigate to the **Bots** page and open your bot.
2. Go to the **Settings** tab.
3. Find the **Memory Provider** dropdown.
4. Select the provider you created.
5. Click **Save**.

If no memory provider is selected, the bot will not use that provider configuration in its runtime settings.

---

## Using Memory After Setup

Once a memory provider is assigned to the bot, you can manage actual memories from the bot's **Memory** tab:

- Create memories manually
- Extract memories from conversations
- Search, edit, and delete memories
- Compact or rebuild the memory store

For day-to-day memory operations, continue with [Bot Memory Management](/getting-started/memory.md).
