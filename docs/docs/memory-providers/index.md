# Memory Providers

Memoh uses a **Memory Provider** to define how a bot stores, retrieves, and manages long-term memory. A bot can bind one memory provider in its **Settings** tab, and that provider becomes the backend for memory extraction and memory search.

## Available Providers

Memoh currently includes the following memory provider:

- [Built-in](/memory-providers/builtin.md): The default memory system included with Memoh.

More provider types may be added in future versions, but right now `builtin` is the only supported provider type in the product and web UI.

---

## Basic Flow

1. Open the **Memory Providers** page from the sidebar.
2. Create a provider instance using one of the supported provider types.
3. Configure the provider settings.
4. Open a bot's **Settings** tab and assign that provider in **Memory Provider**.
5. Manage actual memories from the bot's **Memory** tab.

---

## Next Steps

- To configure the currently supported provider, continue with [Built-in Memory Provider](/memory-providers/builtin.md).
- To manage memory entries after the provider is assigned, see [Bot Memory Management](/getting-started/memory.md).
