package com.example.compute;

import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;

// v679: this file lives under src/test/java/..., a test source set, so it is ignored
// for normal indexing and read only for its references: one test_ref fact naming
// Calculator, FancyCalculator and fib, without JUnit's assertEquals or the test's own names.
public class CalculatorTest {

    @Test
    public void testFib() {
        Calculator calc = new FancyCalculator();
        assertEquals(5, calc.fib(5));
    }
}
