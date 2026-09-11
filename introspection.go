package graphql

// introspectionOptions returns the bindings for the introspection schema.
// They are installed by NewSchema after user options.
func introspectionOptions() []SchemaOption {
	return nil
}
