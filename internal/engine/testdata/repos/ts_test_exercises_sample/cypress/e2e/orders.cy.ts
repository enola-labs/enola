describe("orders", () => {
  it("loads one order", () => {
    cy.request("GET", "/api/orders/42");
    cy.request("GET", "/api/refunds");
  });
});
